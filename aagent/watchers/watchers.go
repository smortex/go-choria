package watchers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/tidwall/gjson"

	"github.com/choria-io/go-choria/aagent/watchers/watcher"
	"github.com/choria-io/go-choria/choria"
)

type State int

// Watcher is anything that can be used to watch the system for events
type Watcher interface {
	Name() string
	Type() string
	Run(context.Context, *sync.WaitGroup)
	NotifyStateChance()
	CurrentState() interface{}
	AnnounceInterval() time.Duration
	Delete()
}

// Machine is a Choria Machine
type Machine interface {
	Name() string
	State() string
	Directory() string
	Transition(t string, args ...interface{}) error
	NotifyWatcherState(string, interface{})
	Watchers() []*WatcherDef
	Identity() string
	InstanceID() string
	Version() string
	TimeStampSeconds() int64
	TextFileDirectory() string
	OverrideData() ([]byte, error)
	ChoriaStatusFile() (string, int)
	Debugf(name string, format string, args ...interface{})
	Infof(name string, format string, args ...interface{})
	Errorf(name string, format string, args ...interface{})
}

// Manager manages all the defined watchers in a specific machine
// implements machine.WatcherManager
type Manager struct {
	watchers map[string]Watcher
	machine  Machine
	sync.Mutex
}

// WatcherConstructor creates a new watcher plugin
type WatcherConstructor interface {
	New(machine watcher.Machine, name string, states []string, failEvent string, successEvent string, interval string, ai time.Duration, properties map[string]interface{}) (interface{}, error)
	Type() string
	EventType() string
	UnmarshalNotification(n []byte) (interface{}, error)
}

var (
	plugins map[string]WatcherConstructor

	mu sync.Mutex
)

// RegisterWatcherPlugin registers a new type of watcher
func RegisterWatcherPlugin(name string, plugin WatcherConstructor) error {
	mu.Lock()
	defer mu.Unlock()

	if plugins == nil {
		plugins = map[string]WatcherConstructor{}
	}

	_, exit := plugins[plugin.Type()]
	if exit {
		return fmt.Errorf("plugin %q already exist", plugin.Type())
	}

	plugins[plugin.Type()] = plugin

	choria.BuildInfo().RegisterMachineWatcher(name)

	return nil
}

func New() *Manager {
	return &Manager{
		watchers: make(map[string]Watcher),
	}
}

func ParseWatcherState(state []byte) (interface{}, error) {
	r := gjson.GetBytes(state, "protocol")
	if !r.Exists() {
		return nil, fmt.Errorf("no protocol header in state json")
	}

	proto := r.String()
	var plugin WatcherConstructor

	mu.Lock()
	for _, w := range plugins {
		if w.EventType() == proto {
			plugin = w
		}
	}
	mu.Unlock()

	if plugin == nil {
		return nil, fmt.Errorf("unknown event type %q", proto)
	}

	return plugin.UnmarshalNotification(state)
}

// Delete gets called before a watcher is being deleted after
// its files were removed from disk
func (m *Manager) Delete() {
	m.Lock()
	defer m.Unlock()

	for _, w := range m.watchers {
		w.Delete()
	}
}

// SetMachine supplies the machine this manager will manage
func (m *Manager) SetMachine(t interface{}) (err error) {
	machine, ok := t.(Machine)
	if !ok {
		return fmt.Errorf("supplied machine does not implement watchers.Machine")
	}

	m.machine = machine

	return nil
}

// AddWatcher adds a watcher to a managed machine
func (m *Manager) AddWatcher(w Watcher) error {
	m.Lock()
	defer m.Unlock()

	_, ok := m.watchers[w.Name()]
	if ok {
		m.machine.Errorf("manager", "Already have a watcher %s", w.Name())
		return fmt.Errorf("watcher %s already exist", w.Name())
	}

	m.watchers[w.Name()] = w

	return nil
}

// WatcherState retrieves the current status for a given watcher, boolean result is false for unknown watchers
func (m *Manager) WatcherState(watcher string) (interface{}, bool) {
	m.Lock()
	defer m.Unlock()
	w, ok := m.watchers[watcher]
	if !ok {
		return nil, false
	}

	return w.CurrentState(), true
}

func (m *Manager) configureWatchers() (err error) {
	for _, w := range m.machine.Watchers() {
		err = w.ParseAnnounceInterval()
		if err != nil {
			return fmt.Errorf("could not create %s watcher '%s': %s", w.Type, w.Name, err)
		}

		m.machine.Infof("manager", "Starting %s watcher %s", w.Type, w.Name)

		var watcher Watcher
		var err error
		var ok bool

		mu.Lock()
		plugin, known := plugins[w.Type]
		mu.Unlock()
		if !known {
			return fmt.Errorf("unknown watcher '%s'", w.Type)
		}

		wi, err := plugin.New(m.machine, w.Name, w.StateMatch, w.FailTransition, w.SuccessTransition, w.Interval, w.AnnounceDuration, w.Properties)
		if err != nil {
			return fmt.Errorf("could not create %s watcher '%s': %s", w.Type, w.Name, err)
		}

		watcher, ok = wi.(Watcher)
		if !ok {
			return fmt.Errorf("%q watcher is not a valid watcher", w.Type)
		}

		err = m.AddWatcher(watcher)
		if err != nil {
			return err
		}
	}

	return nil
}

// Run starts all the defined watchers and periodically announce
// their state based on AnnounceInterval
func (m *Manager) Run(ctx context.Context, wg *sync.WaitGroup) error {
	if m.machine == nil {
		return fmt.Errorf("manager requires a machine to manage")
	}

	err := m.configureWatchers()
	if err != nil {
		return err
	}

	for _, watcher := range m.watchers {
		wg.Add(1)
		go watcher.Run(ctx, wg)

		if watcher.AnnounceInterval() > 0 {
			wg.Add(1)
			go m.announceWatcherState(ctx, wg, watcher)
		}
	}

	return nil
}

func (m *Manager) announceWatcherState(ctx context.Context, wg *sync.WaitGroup, w Watcher) {
	defer wg.Done()

	announceTick := time.NewTicker(w.AnnounceInterval())

	for {
		select {
		case <-announceTick.C:
			m.machine.NotifyWatcherState(w.Name(), w.CurrentState())
		case <-ctx.Done():
			m.machine.Infof("manager", "Stopping on context interrupt")
			return
		}
	}
}

// NotifyStateChance implements machine.WatcherManager
func (m *Manager) NotifyStateChance() {
	m.Lock()
	defer m.Unlock()

	for _, watcher := range m.watchers {
		watcher.NotifyStateChance()
	}
}
