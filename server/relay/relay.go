package relay

import (
	"database/sql"
	"fmt"
	"net"

	"github.com/rumblefrog/source-chat-relay/server/entity"
	"github.com/rumblefrog/source-chat-relay/server/filter"
	"github.com/rumblefrog/source-chat-relay/server/packet"

	"github.com/rumblefrog/source-chat-relay/server/protocol"
	"github.com/sirupsen/logrus"
)

var Instance *Relay

type Relay struct {
	Clients  map[*RelayClient]bool
	Router   chan protocol.Deliverable
	Bot      chan protocol.Deliverable
	Listener net.Listener
}

type RelayClient struct {
	Socket   net.Conn
	Data     chan []byte
	ID       string
	Hostname string
}

func (c *RelayClient) Authenticated() bool {
	return len(c.ID) != 0
}

func NewRelay() *Relay {
	return &Relay{
		Clients: make(map[*RelayClient]bool),
		Router:  make(chan protocol.Deliverable),
		Bot:     make(chan protocol.Deliverable),
	}
}

func (r *Relay) Listen(port int) error {
	var err error

	r.Listener, err = net.Listen("tcp", fmt.Sprintf(":%d", port))

	if err != nil {
		return err
	}

	go r.StartRouting()
	go r.ProcessConnections()

	return nil
}

func (r *Relay) StartRouting() {
	for {
		select {
		case message := <-r.Router:
			if filter.IsInFilter(message.Content()) {
				return
			}

			// Iterate connected clients
			for client := range r.Clients {
				tEntity, err := entity.GetEntity(client.ID)

				if err != nil {
					continue
				}

				if client.ID != message.Author() &&
					tEntity.ReceiveIntersectsWith(entity.DeliverableSendChannels(message)) {
					select {
					case client.Data <- message.Marshal():
					default:
						close(client.Data)
						delete(r.Clients, client)
					}
				}
			}

			// Push to bot channel and it'll iterate Discord channels
			r.Bot <- message
		}
	}
}

func (r *Relay) ProcessConnections() {
	for {
		conn, err := r.Listener.Accept()

		if err != nil {
			logrus.WithField("error", err).Warn("Unable to accept connection")
			return
		}

		logrus.WithField("address", conn.RemoteAddr()).Info("A client connected")

		client := &RelayClient{
			Socket: conn,
			Data:   make(chan []byte),
		}

		r.AddClient(client)

		go r.ListenClientReceive(client)
		go r.ListenClientSend(client)
	}
}

func (r *Relay) ListenClientReceive(c *RelayClient) {
	for {
		buffer := make([]byte, protocol.MAX_BUFFER_LENGTH)

		length, err := c.Socket.Read(buffer)

		if err != nil {
			r.RemoveClient(c)
			c.Socket.Close()
			break
		}

		if length > 0 {
			buffer = buffer[:length]

			r.HandlePacket(c, buffer)
		}
	}
}

func (r *Relay) ListenClientSend(c *RelayClient) {
	defer c.Socket.Close()

	for {
		select {
		case message, ok := <-c.Data:
			if !ok {
				// Exit for loop, execute the defer
				return
			}

			c.Socket.Write(message)
		}
	}
}

func (r *Relay) HandlePacket(client *RelayClient, buffer []byte) {
	reader := packet.NewPacketReader(buffer)

	base := protocol.ParseBaseMessage(reader)

	if base.Type == protocol.MessageAuthenticate {
		authenticateMessage := protocol.ParseAuthenticateMessage(base, reader)
		authenticateResponseMessage := &protocol.AuthenticateMessageResponse{}

		if len(authenticateMessage.Token) == 0 || len(authenticateMessage.Hostname) == 0 {
			authenticateResponseMessage.Response = protocol.AuthenticateDenied

			client.Socket.Write(authenticateResponseMessage.Marshal())

			return
		}

		r.AuthenticateClient(client, authenticateMessage)

		authenticateResponseMessage.Response = protocol.AuthenticateSuccess

		client.Socket.Write(authenticateResponseMessage.Marshal())

		return
	}

	// Switch case for everything else that requires auth prior

	if !client.Authenticated() {
		return
	}

	base.SenderID = client.ID
	base.Hostname = client.Hostname

	switch base.Type {
	case protocol.MessageChat:
		r.Router <- protocol.ParseChatMessage(base, reader)
	case protocol.MessageEvent:
		r.Router <- protocol.ParseEventMessage(base, reader)
	default:
		// Malformed packet, we should not get anything else
		r.RemoveClient(client)
		client.Socket.Close()
	}
}

func (r *Relay) AddClient(c *RelayClient) {
	r.Clients[c] = true
}

func (r *Relay) RemoveClient(c *RelayClient) {
	if _, ok := r.Clients[c]; ok {
		close(c.Data)
		delete(r.Clients, c)
	}
}

func (r *Relay) AuthenticateClient(c *RelayClient, packet *protocol.AuthenticateMessage) {
	tEntity, err := entity.GetEntity(packet.Token)

	if err == sql.ErrNoRows {
		tEntity = &entity.Entity{
			ID: packet.Token,
		}

		if err = tEntity.Insert(); err != nil {
			logrus.WithField("error", err).Warn("Failed to create entity in database")
			return
		}
	} else if err != nil {
		logrus.WithField("error", err).Warn("Failed to fetch entity from database")
	}

	// Update database with new name upon auth
	tEntity.SetDisplayName(packet.Hostname)

	c.ID = string(packet.Token)
	c.Hostname = packet.Hostname
}
