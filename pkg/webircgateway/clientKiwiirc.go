package webircgateway

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/igm/sockjs-go/sockjs"
	"github.com/orcaman/concurrent-map"
)

type Channel struct {
	Conn         sockjs.Session
	Client       *Client
	Id           string
	waitForClose chan bool
	ClosedLock   sync.Mutex
	Closed       bool
}

var nextChannelID int

func makeChannel(chanID string, ws sockjs.Session) *Channel {
	client := NewClient()

	originHeader := strings.ToLower(ws.Request().Header.Get("Origin"))
	if !isClientOriginAllowed(originHeader) {
		client.Log(2, "Origin %s not allowed. Closing connection", originHeader)
		ws.Close(0, "Origin not allowed")
		return nil
	}

	client.RemoteAddr = GetRemoteAddressFromRequest(ws.Request()).String()

	clientHostnames, err := net.LookupAddr(client.RemoteAddr)
	if err != nil || len(clientHostnames) == 0 {
		client.RemoteHostname = client.RemoteAddr
	} else {
		// FQDNs include a . at the end. Strip it out
		potentialHostname := strings.Trim(clientHostnames[0], ".")

		// Must check that the resolved hostname also resolves back to the users IP
		addr, err := net.LookupIP(potentialHostname)
		if err == nil && len(addr) == 1 && addr[0].String() == client.RemoteAddr {
			client.RemoteHostname = potentialHostname
		} else {
			client.RemoteHostname = client.RemoteAddr
		}
	}

	if isRequestSecure(ws.Request()) {
		client.Tags["secure"] = ""
	}

	// This doesn't make sense to have since the remote port may change between requests. Only
	// here for testing purposes for now.
	_, remoteAddrPort, _ := net.SplitHostPort(ws.Request().RemoteAddr)
	client.Tags["remote-port"] = remoteAddrPort

	client.Log(2, "New kiwi channel from %s %s", client.RemoteAddr, client.RemoteHostname)

	channel := &Channel{
		Id:           chanID,
		Client:       client,
		Conn:         ws,
		waitForClose: make(chan bool),
		Closed:       false,
	}

	go channel.listenForSignals()

	return channel
}

func (c *Channel) listenForSignals() {
	for {
		signal, ok := <-c.Client.Signals
		if !ok {
			break
		}
		c.Client.Log(1, "signal:%s %s", signal[0], signal[1])
		if signal[0] == "state" {
			if signal[1] == "connected" {
				c.Conn.Send(fmt.Sprintf(":%s control connected", c.Id))
			} else if signal[1] == "closed" {
				c.Conn.Send(fmt.Sprintf(":%s control closed %s", c.Id, signal[2]))
			}
		}

		if signal[0] == "data" {
			toSend := strings.Trim(signal[1], "\r\n")
			c.Conn.Send(fmt.Sprintf(":%s %s", c.Id, toSend))
		}
	}

	c.ClosedLock.Lock()

	c.Closed = true
	close(c.Client.Recv)
	close(c.waitForClose)

	c.ClosedLock.Unlock()
}

func (c *Channel) handleIncomingLine(line string) {
	c.ClosedLock.Lock()

	if !c.Closed {
		c.Client.Recv <- line
	}

	c.ClosedLock.Unlock()
}

func kiwiircHTTPHandler(router *http.ServeMux) {
	handler := sockjs.NewHandler("/webirc/kiwiirc", sockjs.DefaultOptions, kiwiircHandler)
	router.Handle("/webirc/kiwiirc/", handler)
}

func kiwiircHandler(session sockjs.Session) {
	// Don't let a single users error kill the entire service for everyone
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[ERROR] Recovered from %s\n%s", r, debug.Stack())
		}
	}()

	channels := cmap.New()

	// Read from sockjs
	go func() {
		for {
			msg, err := session.Recv()
			if err == nil && len(msg) > 0 {
				idEnd := strings.Index(msg, " ")
				if idEnd == -1 {
					// msg is in the form of ":chanId"
					chanID := msg[1:]

					_, channelExists := channels.Get(chanID)
					if !channelExists {
						channel := makeChannel(chanID, session)
						if channel == nil {
							continue
						}
						channels.Set(chanID, channel)

						// When the channel closes, remove it from the map again
						go func() {
							<-channel.waitForClose
							channel.Client.Log(2, "Removing channel from connection")
							channels.Remove(chanID)
						}()
					}

					session.Send(":" + chanID)

				} else {
					// msg is in the form of ":chanId data"
					chanID := msg[1:idEnd]
					data := msg[idEnd+1:]

					channel, channelExists := channels.Get(chanID)
					if channelExists {
						c := channel.(*Channel)
						c.handleIncomingLine(data)
					}
				}
			} else if err != nil {
				logOut(1, "kiwi connection closed (%s)", err.Error())
				break
			}
		}

		for channel := range channels.Iter() {
			c := channel.Val.(*Channel)
			c.Closed = true
			c.Client.StartShutdown("client_closed")
		}
	}()
}
