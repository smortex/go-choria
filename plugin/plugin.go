package plugin

import (
	"fmt"
	"io/ioutil"
	"sort"
	"time"

	"github.com/choria-io/go-choria/internal/fs"
	"github.com/ghodss/yaml"
)

// Type are types of choria plugin
type Type int

// List is a list of plugins to load
type List struct {
	Plugins []*Plugin
}

// Plugin is an individual plugin
type Plugin struct {
	Name string
	Repo string
}

const (
	// UnknownPlugin is a unknown plugin type
	UnknownPlugin Type = iota

	// AgentProviderPlugin is a plugin that provide types of agents to Choria
	AgentProviderPlugin

	// AgentPlugin is a type of agent
	AgentPlugin

	// ProvisionTargetResolverPlugin is a plugin that helps provisioning mode Choria find its broker
	ProvisionTargetResolverPlugin

	// ConfigMutatorPlugin is a plugin that can dynamically adjust
	// configuration based on local site conditions
	ConfigMutatorPlugin

	// MachineWatcherPlugin is a plugin that adds a Autonomous Agent Watcher
	MachineWatcherPlugin

	// DataPlugin is a plugin that provides data to choria
	DataPlugin
)

// Pluggable is a Choria Plugin
type Pluggable interface {
	// PluginInstance is any structure that implements the plugin, should be right type for the kind of plugin
	PluginInstance() interface{}

	// PluginName is a human friendly name for the plugin
	PluginName() string

	// PluginType is the type of the plugin, to match plugin.Type
	PluginType() Type

	// PluginVersion is the version of the plugin
	PluginVersion() string
}

// Register registers a type of plugin into the choria server
func Register(name string, plugin Pluggable) error {
	var err error

	switch Type(plugin.PluginType()) {
	case AgentProviderPlugin:
		err = registerAgentProviderPlugin(name, plugin)

	case AgentPlugin:
		err = registerAgentPlugin(name, plugin)

	case ProvisionTargetResolverPlugin:
		err = registerProvisionTargetResolverPlugin(name, plugin)

	case ConfigMutatorPlugin:
		err = registerConfigMutator(name, plugin)

	case MachineWatcherPlugin:
		err = registerWatcherPlugin(name, plugin)

	case DataPlugin:
		err = registerDataPlugin(name, plugin)

	default:
		err = fmt.Errorf("unknown plugin type %d from %s", plugin.PluginType(), name)
	}

	return err
}

// Load loads a plugin list from file
func Load(file string) (*List, error) {
	rawdat := make(map[string]string)
	input, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}

	err = yaml.Unmarshal(input, &rawdat)
	if err != nil {
		return nil, fmt.Errorf("could not parse yaml: %s", err)
	}

	list := &List{Plugins: []*Plugin{}}
	for k, v := range rawdat {
		list.Plugins = append(list.Plugins, &Plugin{Name: k, Repo: v})
	}

	sort.Slice(list.Plugins, func(i, j int) bool {
		return list.Plugins[i].Name < list.Plugins[j].Name
	})

	return list, err
}

// Now is the current time
func (p *Plugin) Now() string {
	return time.Now().String()
}

// Loader is the loader go code
func (p *Plugin) Loader() (string, error) {
	out, err := fs.ExecuteTemplate("plugin/plugin.templ", p, nil)
	if err != nil {
		return "", err
	}

	return string(out), err
}
