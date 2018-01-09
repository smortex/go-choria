package mcorpc

import (
	"encoding/json"

	"github.com/choria-io/go-choria/build"
	"github.com/choria-io/go-choria/choria"
	"github.com/choria-io/go-choria/choria/connectortest"
	"github.com/choria-io/go-protocol/protocol"
	"github.com/choria-io/go-choria/server/agents"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/tidwall/gjson"

	"testing"
)

func TestFileContent(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Server/Agents/McoRPC")
}

var _ = Describe("Server/Agents/McoRPC", func() {
	var (
		agent  *Agent
		fw     *choria.Framework
		msg    *choria.Message
		req    protocol.Request
		outbox = make(chan *agents.AgentReply, 1)
		ci     = &connectortest.ConnectorInfo{}
		err    error
	)

	BeforeEach(func() {
		protocol.Secure = "false"
		build.TLS = "false"

		config, err := choria.NewConfig("/dev/null")
		Expect(err).ToNot(HaveOccurred())
		config.LogLevel = "fatal"
		fw, err = choria.NewWithConfig(config)
		Expect(err).ToNot(HaveOccurred())

		metadata := &agents.Metadata{Name: "test"}
		agent = New("testing", metadata, fw, fw.Logger("test"))
	})

	It("Should have correct constants", func() {
		Expect(OK).To(Equal(StatusCode(0)))
		Expect(Aborted).To(Equal(StatusCode(1)))
		Expect(UnknownAction).To(Equal(StatusCode(2)))
		Expect(MissingData).To(Equal(StatusCode(3)))
		Expect(InvalidData).To(Equal(StatusCode(4)))
		Expect(UnknownError).To(Equal(StatusCode(5)))
	})

	var _ = Describe("RegisterAction", func() {
		It("Should fail if the action already exist", func() {
			action := func(req *Request, reply *Reply, agent *Agent, conn choria.ConnectorInfo) {}
			err := agent.RegisterAction("test", action)
			Expect(err).ToNot(HaveOccurred())
			err = agent.RegisterAction("test", action)
			Expect(err).To(MatchError("Cannot register action test, it already exist"))
		})
	})

	var _ = Describe("HandleMessage", func() {
		BeforeEach(func() {
			req, err = fw.NewRequest(protocol.RequestV1, "test", "test.example.net", "choria=rip.mcollective", 60, fw.NewRequestID(), "mcollective")
			Expect(err).ToNot(HaveOccurred())
			msg, err = choria.NewMessageFromRequest(req, "dev.null", fw)
			Expect(err).ToNot(HaveOccurred())
		})

		It("Should handle bad incoming data", func() {
			msg.Payload = ""
			agent.HandleMessage(msg, req, ci, outbox)

			reply := <-outbox
			Expect(gjson.GetBytes(reply.Body, "statusmsg").String()).To(Equal("Could not process request: Could not parse incoming message as a MCollective SimpleRPC Request: unexpected end of JSON input"))
			Expect(gjson.GetBytes(reply.Body, "statuscode").Int()).To(Equal(int64(4)))
		})

		It("Should handle unknown actions", func() {
			msg.Payload = `{"agent":"test", "action":"nonexisting"}`
			agent.HandleMessage(msg, req, ci, outbox)

			reply := <-outbox
			Expect(gjson.GetBytes(reply.Body, "statusmsg").String()).To(Equal("Unknown action nonexisting for agent test"))
			Expect(gjson.GetBytes(reply.Body, "statuscode").Int()).To(Equal(int64(2)))
		})

		It("Should call the action", func() {
			action := func(req *Request, reply *Reply, agent *Agent, conn choria.ConnectorInfo) {
				d := make(map[string]string)
				d["test"] = "hello world"
				reply.Data = &d
			}

			agent.RegisterAction("test", action)
			msg.Payload = `{"agent":"test", "action":"test"}`
			agent.HandleMessage(msg, req, ci, outbox)

			reply := <-outbox
			Expect(gjson.GetBytes(reply.Body, "statusmsg").String()).To(Equal("OK"))
			Expect(gjson.GetBytes(reply.Body, "statuscode").Int()).To(Equal(int64(0)))
			Expect(gjson.GetBytes(reply.Body, "data.test").String()).To(Equal("hello world"))
		})
	})

	var _ = Describe("publish", func() {
		It("Should handle bad data", func() {
			reply := &Reply{
				Data: outbox,
			}

			agent.publish(reply, msg, req, outbox)
			out := <-outbox
			Expect(out.Error).To(MatchError("json: unsupported type: chan *agents.AgentReply"))
		})

		PIt("Should publish good messages")
	})

	var _ = Describe("ParseRequestData", func() {
		It("Should handle valid data correctly", func() {
			req := &Request{
				Data: json.RawMessage(`{"hello":"world"}`),
			}

			reply := &Reply{}

			var params struct {
				Hello string `json:"hello"`
			}

			ok := ParseRequestData(&params, req, reply)

			Expect(ok).To(BeTrue())
			Expect(params.Hello).To(Equal("world"))
		})

		It("Should handle invalid data correctly", func() {
			req := &Request{
				Agent:  "test",
				Action: "will_fail",
				Data:   json.RawMessage(`fail`),
			}

			reply := &Reply{}

			var params struct {
				Hello string `json:"hello"`
			}

			ok := ParseRequestData(&params, req, reply)

			Expect(ok).To(BeFalse())
			Expect(reply.Statuscode).To(Equal(InvalidData))
			Expect(reply.Statusmsg).To(Equal("Could not parse request data for test#will_fail: invalid character 'i' in literal false (expecting 'l')"))
		})
	})

	var _ = Describe("newReply", func() {
		It("Should set the correct starting code and message", func() {
			r := agent.newReply()
			Expect(r.Statuscode).To(Equal(OK))
			Expect(r.Statusmsg).To(Equal("OK"))
		})
	})
})
