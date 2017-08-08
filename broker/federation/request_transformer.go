package federation

import (
	"fmt"

	log "github.com/sirupsen/logrus"
)

func NewChoriaRequestTransformer(workers int, capacity int, broker *FederationBroker, logger *log.Entry) (*pooledWorker, error) {
	worker, err := PooledWorkerFactory("choria_request_transformer", workers, Unconnected, capacity, broker, logger, func(self *pooledWorker, i int, logger *log.Entry) {
		defer self.wg.Done()

		for {
			var cm chainmessage

			select {
			case cm = <-self.in:
			case <-self.done:
				logger.Infof("Worker routine %s exiting", self.Name())
				return
			}

			req, federated := cm.Message.FederationRequestID()
			if !federated {
				logger.Errorf("Received a message from %s that is not federated", cm.Message.SenderID())
				continue
			}

			targets, _ := cm.Message.FederationTargets()
			if len(targets) == 0 {
				logger.Errorf("Received a message %s from %s that does not have any targets", req, cm.Message.SenderID())
				continue
			}

			replyto := cm.Message.ReplyTo()
			if replyto == "" {
				logger.Errorf("Received a message %s with no reply-to set", req)
				continue
			}

			cm.Seen = append(cm.Seen, self.Name())
			cm.RequestID = req
			cm.Targets = targets

			cm.Message.SetFederationTargets([]string{})
			cm.Message.SetFederationReplyTo(replyto)
			cm.Message.SetReplyTo(fmt.Sprintf("choria.federation.%s.collective", self.broker.Name))

			logger.Infof("Received request message '%s' via %s with %d targets", cm.RequestID, cm.Message.SenderID(), len(cm.Targets))

			self.out <- cm
		}

	})

	return worker, err
}
