package cherryDiscovery

import (
	"fmt"
	cfacade "github.com/cherry-game/cherry/facade"
	clog "github.com/cherry-game/cherry/logger"
	cnats "github.com/cherry-game/cherry/net/nats"
	cproto "github.com/cherry-game/cherry/net/proto"
	cprofile "github.com/cherry-game/cherry/profile"
	"github.com/nats-io/nats.go"
	"time"
)

// DiscoveryNATS master节点模式(master为单节点)
// 先启动一个master节点
// 其他节点启动时Request(cherry.discovery.register)，到master节点注册
// master节点subscribe(cherry.discovery.register)，返回已注册节点列表
// master节点publish(cherry.discovery.addMember)，当前已注册的节点到
// 所有客户端节点subscribe(cherry.discovery.addMember)，接收新节点
// 所有节点subscribe(cherry.discovery.unregister)，退出时注销节点
type DiscoveryNATS struct {
	DiscoveryDefault
	app               cfacade.IApplication
	natsConn          *cnats.Conn
	masterMember      cfacade.IMember
	registerSubject   string
	unregisterSubject string
	addSubject        string
}

func (m *DiscoveryNATS) Name() string {
	return "nats"
}

func (m *DiscoveryNATS) Load(app cfacade.IApplication) {
	m.app = app
	m.natsConn = cnats.New()
	m.natsConn.Connect()

	//get nats config
	config := cprofile.GetConfig("cluster").GetConfig(m.Name())
	if config.LastError() != nil {
		clog.Fatalf("nats config parameter not found. err = %v", config.LastError())
	}

	// get master node id
	masterId := config.GetString("master_node_id")
	if masterId == "" {
		clog.Fatal("master node id not in config.")
	}

	// load master node config
	masterNode, err := cprofile.LoadNode(masterId)
	if err != nil {
		clog.Fatal(err)
	}

	m.masterMember = &cproto.Member{
		NodeId:   masterNode.NodeId(),
		NodeType: masterNode.NodeType(),
		Address:  masterNode.RpcAddress(),
		Settings: make(map[string]string),
	}

	m.init()
}

func (m *DiscoveryNATS) isMaster() bool {
	return m.app.NodeId() == m.masterMember.GetNodeId()
}

func (m *DiscoveryNATS) isClient() bool {
	return m.app.NodeId() != m.masterMember.GetNodeId()
}

func (m *DiscoveryNATS) init() {
	masterNodeId := m.masterMember.GetNodeId()
	m.registerSubject = fmt.Sprintf("cherry.discovery.%s.register", masterNodeId)
	m.unregisterSubject = fmt.Sprintf("cherry.discovery.%s.unregister", masterNodeId)
	m.addSubject = fmt.Sprintf("cherry.discovery.%s.addMember", masterNodeId)

	m.subscribe(m.unregisterSubject, func(msg *nats.Msg) {
		unregisterMember := &cproto.Member{}
		err := m.app.Serializer().Unmarshal(msg.Data, unregisterMember)
		if err != nil {
			clog.Warnf("err = %s", err)
			return
		}

		if unregisterMember.NodeId == m.app.NodeId() {
			return
		}

		// remove member
		m.RemoveMember(unregisterMember.NodeId)
	})

	m.serverInit()
	m.clientInit()

	clog.Infof("[discovery = %s] is running.", m.Name())
}

func (m *DiscoveryNATS) serverInit() {
	if m.isMaster() == false {
		return
	}

	//addMember master node
	m.AddMember(m.masterMember)

	// subscribe register message
	m.subscribe(m.registerSubject, func(msg *nats.Msg) {
		newMember := &cproto.Member{}
		err := m.app.Serializer().Unmarshal(msg.Data, newMember)
		if err != nil {
			clog.Warnf("IMember Unmarshal[name = %s] error. dataLen = %+v, err = %s",
				m.app.Serializer().Name(),
				len(msg.Data),
				err,
			)
			return
		}

		// addMember new member
		m.AddMember(newMember)

		// response member list
		rspMemberList := &cproto.MemberList{}
		for _, member := range m.memberList {
			if member.GetNodeId() == newMember.GetNodeId() {
				continue
			}

			if member.GetNodeId() == m.app.NodeId() {
				continue
			}

			if protoMember, ok := member.(*cproto.Member); ok {
				rspMemberList.List = append(rspMemberList.List, protoMember)
			}
		}

		rspData, err := m.app.Serializer().Marshal(rspMemberList)
		if err != nil {
			clog.Warnf("marshal fail. err = %s", err)
			return
		}

		// response member list
		err = msg.Respond(rspData)
		if err != nil {
			clog.Warnf("respond fail. err = %s", err)
			return
		}

		// publish addMember new node
		err = m.natsConn.Publish(m.addSubject, msg.Data)
		if err != nil {
			clog.Warnf("publish fail. err = %s", err)
			return
		}
	})
}

func (m *DiscoveryNATS) clientInit() {
	if m.isClient() == false {
		return
	}

	registerMember := &cproto.Member{
		NodeId:   m.app.NodeId(),
		NodeType: m.app.NodeType(),
		Address:  m.app.RpcAddress(),
		Settings: make(map[string]string),
	}

	bytesData, err := m.app.Serializer().Marshal(registerMember)
	if err != nil {
		clog.Warnf("err = %s", err)
		return
	}

	// receive registered node
	m.subscribe(m.addSubject, func(msg *nats.Msg) {
		addMember := &cproto.Member{}
		err := m.app.Serializer().Unmarshal(msg.Data, addMember)
		if err != nil {
			clog.Warnf("err = %s", err)
			return
		}

		if _, ok := m.GetMember(addMember.NodeId); ok == false {
			m.AddMember(addMember)
		}
	})

	for {
		// register current node to master
		rsp, err := m.natsConn.Request(m.registerSubject, bytesData)
		if err != nil {
			clog.Warnf("register node to [master = %s] fail. [address = %s] [err = %s]",
				m.masterMember.GetNodeId(),
				m.natsConn.Address(),
				err,
			)
			time.Sleep(m.natsConn.ReconnectDelay())
			continue
		}

		clog.Infof("register node to [master = %s]. [member = %s]",
			m.masterMember.GetNodeId(),
			registerMember,
		)

		memberList := cproto.MemberList{}
		err = m.app.Serializer().Unmarshal(rsp.Data, &memberList)
		if err != nil {
			clog.Warnf("err = %s", err)
			time.Sleep(m.natsConn.ReconnectDelay())
			continue
		}

		for _, member := range memberList.GetList() {
			m.AddMember(member)
		}

		break
	}
}

func (m *DiscoveryNATS) Stop() {
	if m.isClient() {
		thisMember := &cproto.Member{
			NodeId: m.app.NodeId(),
		}

		bytesData, err := m.app.Serializer().Marshal(thisMember)
		if err != nil {
			clog.Warnf("marshal fail. err = %s", err)
			return
		}

		err = m.natsConn.Publish(m.unregisterSubject, bytesData)
		if err != nil {
			clog.Warnf("publish fail. err = %s", err)
			return
		}

		clog.Debugf("[nodeId = %s] unregister node to [master = %s]",
			m.app.NodeId(),
			m.masterMember.GetNodeId(),
		)
	}

	m.natsConn.Close()
}

func (m *DiscoveryNATS) subscribe(subject string, cb nats.MsgHandler) {
	_, err := m.natsConn.Subscribe(subject, cb)
	if err != nil {
		clog.Warnf("subscribe fail. err = %s", err)
		return
	}
}