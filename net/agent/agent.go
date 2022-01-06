package cherryAgent

import (
	"fmt"
	"github.com/cherry-game/cherry/facade"
	cherryLogger "github.com/cherry-game/cherry/logger"
	"github.com/cherry-game/cherry/net/command"
	"github.com/cherry-game/cherry/net/message"
	"github.com/cherry-game/cherry/net/packet"
	cherryProto "github.com/cherry-game/cherry/net/proto"
	"github.com/cherry-game/cherry/net/session"
	cherryProfile "github.com/cherry-game/cherry/profile"
	"sync"
	"sync/atomic"
	"time"
)

const (
	WriteBacklog = 64
)

type (
	Options struct {
		Heartbeat time.Duration                                // heartbeat(sec)
		Commands  map[cherryPacket.Type]cherryCommand.ICommand // commands
	}

	Agent struct {
		sync.RWMutex
		*Options
		cherryFacade.IApplication
		Session *cherrySession.Session // session
		conn    cherryFacade.INetConn  // low-level conn fd
		chDie   chan bool              // wait for close
		chSend  chan pendingMessage    // push message queue
		chWrite chan []byte            // push bytes queue
		lastAt  int64                  // last heartbeat unix time stamp
	}

	pendingMessage struct {
		typ     cherryMessage.Type // message type
		route   string             // message route(push)
		mid     uint               // response message id(response)
		payload interface{}        // payload
		err     bool               // if its an error
	}
)

func (p *pendingMessage) String() string {
	return fmt.Sprintf("typ = %d, route = %s, mid =%d, payload=%v", p.typ, p.route, p.mid, p.payload)
}

func NewAgent(app cherryFacade.IApplication, conn cherryFacade.INetConn, opts *Options) *Agent {
	agent := &Agent{
		IApplication: app,
		Options:      opts,
		conn:         conn,
		chDie:        make(chan bool),
		chSend:       make(chan pendingMessage, WriteBacklog),
		chWrite:      make(chan []byte, WriteBacklog),
	}

	if agent.Heartbeat.Seconds() < 1 {
		agent.Heartbeat = 60 * time.Second
	}

	return agent
}

func (a *Agent) SetLastAt() {
	atomic.StoreInt64(&a.lastAt, time.Now().Unix())
}

func (a *Agent) Push(route string, val interface{}) {
	a.Send(cherryMessage.Push, route, 0, val, false)
}

func (a *Agent) Kick(reason interface{}) {
	bytes, err := a.Marshal(reason)
	if err != nil {
		a.Session.Warnf("[Kick] marshal fail. [reason = %v] [error = %s].", reason, err)
	}

	pkg, err := a.PacketEncode(cherryPacket.Kick, bytes)
	if err != nil {
		a.Session.Warnf("[kick] packet encode error.[reason = %v] [error = %s].", reason, err)
		return
	}

	_, err = a.conn.Write(pkg)
	if err != nil {
		cherryLogger.Warn(err)
	}

	if cherryProfile.Debug() {
		a.Session.Debugf("[Kick] [reason = %v]", reason)
	}
}

func (a *Agent) Response(mid uint, v interface{}, isError ...bool) {
	err := false
	if len(isError) > 0 {
		err = isError[0]
	}

	a.Send(cherryMessage.Response, "", mid, v, err)
}

func (a *Agent) RPC(route string, val interface{}, _ *cherryProto.Response) {
	cherryLogger.Errorf("cluster no implement. [route = %s] [val = %v]", route, val)
}

func (a *Agent) SendRaw(bytes []byte) {
	a.chWrite <- bytes
}

func (a *Agent) RemoteAddr() string {
	if a.conn != nil {
		return a.conn.RemoteAddr().String()
	}

	return ""
}

func (a *Agent) Close() {
	a.Lock()
	defer a.Unlock()

	if a.Session.State() == cherrySession.Closed {
		return
	}

	a.Session.SetState(cherrySession.Closed)
	a.Session.OnCloseProcess()

	a.chDie <- true

	if err := a.conn.Close(); err != nil {
		a.Session.Debugf("session close error[%s]", err)
	}
}

func (a *Agent) Send(typ cherryMessage.Type, route string, mid uint, v interface{}, isError bool) {
	if a.Session.State() == cherrySession.Closed {
		a.Session.Warnf("[send] session status == Closed")
		return
	}

	if len(a.chSend) >= WriteBacklog {
		a.Session.Warnf("[send] session send buffer exceed")
		return
	}

	pending := pendingMessage{typ: typ, mid: mid, route: route, payload: v, err: isError}
	a.chSend <- pending
}

func (a *Agent) Run() {
	go a.read()
	go a.write()
}

func (a *Agent) read() {
	defer func() {
		a.Close()
	}()

	for {
		msg, err := a.conn.GetNextMessage()
		if err != nil {
			return
		}

		packets, err := a.PacketDecode(msg)
		if err != nil {
			a.Session.Warnf("packet decoder error. error[%s], msg[%s]", err, msg)
			continue
		}

		if len(packets) < 1 {
			continue
		}

		for _, packet := range packets {
			a.processPacket(packet)
		}
	}
}

func (a *Agent) write() {
	ticker := time.NewTicker(a.Heartbeat)
	defer func() {
		a.Session.Debugf("close session. [sid = %s]", a.Session.SID())

		ticker.Stop()
		a.Close()

		close(a.chSend)
		close(a.chWrite)
		//close(a.chDie)
	}()

	for {
		select {
		case <-a.chDie:
			return

		case <-ticker.C:
			deadline := time.Now().Add(-a.Heartbeat).Unix()
			if a.lastAt < deadline {
				a.Session.Debug("check heartbeat timeout.")
				return
			}
		case bytes := <-a.chWrite:
			_, err := a.conn.Write(bytes)
			if err != nil {
				cherryLogger.Warn(err)
				return
			}

		case data := <-a.chSend:
			payload, err := a.Marshal(data.payload)
			if err != nil {
				a.Session.Debugf("message serializer error. data[%s]", data.String())
				return
			}

			// construct message and encode
			m := &cherryMessage.Message{
				Type:  data.typ,
				ID:    data.mid,
				Route: data.route,
				Data:  payload,
				Error: data.err,
			}

			// encode message
			em, err := cherryMessage.Encode(m)
			if err != nil {
				cherryLogger.Warn(err)
				break
			}

			// encode packet
			p, err := a.PacketEncode(cherryPacket.Data, em)
			if err != nil {
				cherryLogger.Warn(err)
				break
			}
			a.chWrite <- p
		}
	}
}

func (a *Agent) processPacket(packet cherryFacade.IPacket) {
	cmd, found := a.Commands[packet.Type()]
	if found == false {
		a.Session.Debugf("packet[%s] type not found.", packet)
		return
	}

	if a.Session == nil {
		cherryLogger.Warnf("session is nil.")
		return
	}

	cmd.Do(a.Session, packet)

	// update last time
	a.SetLastAt()
}
