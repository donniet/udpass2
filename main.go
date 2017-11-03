package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"

	"text/template"

	"github.com/gorilla/websocket"

	"crypto/rand"
)

const (
	idLength       = 24
	maxPayloadSize = 508
)

type userNotFound struct {
	user user
}

func (e userNotFound) Error() string {
	return fmt.Sprintf("user not found: %#v", e.user)
}

type templateData struct {
	SocketURL string
}

type client struct {
	id       string
	conn     *websocket.Conn
	outgoing chan clientCommand
}

type remote struct {
	addr *net.UDPAddr
	conn net.PacketConn
}

type server struct {
	sock     string
	addr     string
	lock     *sync.RWMutex
	upgrader websocket.Upgrader
	clients  map[string]client
	remotes  map[string]remote
	incoming chan clientCommand
	workers  int
	conn     *net.UDPConn
}

type user struct {
	Server string `json:"server"`
	ID     string `json:"id"`
}

type message struct {
	To      *user  `json:"to,omitempty"`
	From    *user  `json:"from,omitempty"`
	Message []byte `json:"message"`
}

type clientCommand struct {
	Send    *message `json:"send,omitempty"`
	Recv    *message `json:"recv,omitempty"`
	Connect *user    `json:"connect,omitempty"`
	Move    string   `json:"move,omitempty"`
	Error   string   `json:"error,omitempty"`
	local   bool
}

type migrate struct {
	// TODO: Migrate message to move a user keeping the userid
}

type serverCommand struct {
	Introduce string   `json:"introduce,omitempty"`
	Send      *message `json:"send,omitempty"`
	Migrate   *migrate `json:"migrate,omitempty"`
}

func randomID() string {
	b := make([]byte, 24)
	if n, err := rand.Read(b); err != nil {
		panic(err)
	} else if n != 24 {
		panic(fmt.Errorf("did not get 24 bytes from random generator"))
	}
	return base64.StdEncoding.EncodeToString(b)
}

func createUser(server string) user {
	return user{
		Server: server,
		ID:     randomID(),
	}
}

func (s *server) getClient(id string) (cl client, ok bool) {
	s.lock.RLock()
	defer s.lock.RUnlock()
	cl, ok = s.clients[id]
	return
}

func (s *server) clientReader(id string) {
	var cl client
	var ok bool
	cl, ok = s.getClient(id)
	if !ok {
		panic(fmt.Errorf("client not found '%s'", id))
	}

	for {
		cmd := clientCommand{}
		if err := cl.conn.ReadJSON(&cmd); err == io.EOF {
			log.Printf("%s", err.Error())
			break
		} else if _, ok = err.(*websocket.CloseError); ok {
			log.Printf("closing %s...", id)
			break
		} else if err != nil {
			log.Printf("%#v", err)
			cl.outgoing <- clientCommand{Error: fmt.Sprintf("%v", err)}
			continue
		}

		if cmd.Send == nil {
			cl.outgoing <- clientCommand{Error: "unknown command"}
			continue
		}
		cmd.Send.From = &user{
			Server: s.addr,
			ID:     id,
		}
		s.incoming <- cmd
	}
}

func (s *server) clientWriter(id string) {
	var cl client
	var ok bool
	cl, ok = s.getClient(id)
	if !ok {
		panic(fmt.Errorf("client not found '%s'", id))
	}

	for {
		cmd := clientCommand{}
		cmd, ok = <-cl.outgoing
		if !ok {
			break
		}

		cl.conn.WriteJSON(cmd)
	}
}

func (s *server) worker() {
	var cmd clientCommand
	var ok bool
	for {
		cmd, ok = <-s.incoming
		if !ok {
			break
		}
		if cmd.Send != nil {
			if err := s.sendOut(*cmd.Send, cmd.local); err != nil {
				log.Printf("%s", err.Error())
			}
		} else {
			log.Printf("only send messages are supported: %#v", cmd)
		}
	}
}

func (s *server) sendOut(snd message, local bool) error {
	if snd.To.Server == s.sock {
		s.lock.RLock()
		defer s.lock.RUnlock()
		c, ok := s.clients[snd.To.ID]
		if !ok {
			return userNotFound{*snd.To}
		}
		c.outgoing <- clientCommand{Recv: &snd}
		return nil
	}

	if local {
		return fmt.Errorf("local message has wrong address: %#v %s", snd, s.addr)
	}

	return s.sendToServer(snd)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if conn, err := s.upgrader.Upgrade(w, r, nil); err == nil {
		u := createUser(s.addr)

		s.lock.Lock()
		s.clients[u.ID] = client{u.ID, conn, make(chan clientCommand)}
		s.lock.Unlock()

		conn.WriteJSON(clientCommand{
			Connect: &u,
		})

		go s.clientReader(u.ID)
		go s.clientWriter(u.ID)
	} else {
		w.WriteHeader(500)
		fmt.Fprintf(w, "%s", err.Error())
	}
}

func (s *server) getRemote(addr string) (remote, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	r, ok := s.remotes[addr]
	if !ok {
		var err error
		r.addr, err = net.ResolveUDPAddr("udp", addr)
		if err != nil {
			return r, err
		}

		s.remotes[addr] = r
	}

	return r, nil
}

func (s *server) sendToServer(msg message) error {
	raddr := msg.To.Server
	if strings.HasPrefix(msg.To.Server, ":") {
		raddr = "localhost" + msg.To.Server
	}
	r, err := s.getRemote(raddr)
	if err != nil {
		panic(err)
	}

	b, err := json.Marshal(serverCommand{Send: &msg})
	if err != nil {
		panic(err)
	}
	_, err = s.conn.WriteTo(b, r.addr)
	log.Printf("writing message: %#v, error %v", serverCommand{Send: &msg}, err)
	return err
}

func (s *server) listener() {
	b := make([]byte, maxPayloadSize)

	var raddr net.Addr

	cn, err := net.ListenPacket("udp", s.sock)
	log.Printf("listening on %s", s.sock)
	if err != nil {
		panic(err)
	}
	defer cn.Close()

	for {
		buf := new(bytes.Buffer)
		n := 0

		for {
			n, raddr, err = cn.ReadFrom(b)
			log.Printf("%d %v %v", n, raddr, err)
			if err != nil {
				log.Printf("%#v", err)
				if err == io.EOF {
					break
				}
			}
			if n > 0 {
				_, err = buf.Write(b[:n])
				if err != nil {
					panic(err)
				}
			}
			if n < maxPayloadSize {
				break
			}
		}

		var scmd serverCommand

		if err = json.Unmarshal(buf.Bytes(), &scmd); err != nil {
			log.Printf("%#v", err)
			continue
		}

		log.Printf("serverMessage: %#v", scmd.Send)

		if scmd.Send != nil {
			s.incoming <- clientCommand{Send: scmd.Send, local: true}
		} else {
			log.Printf("command not processed: %#v", scmd)
		}
	}
}

func NewServer(sock, addr string, servers []string) *server {
	cn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		panic(err)
	}

	ret := &server{
		sock:     sock,
		addr:     addr,
		lock:     &sync.RWMutex{},
		clients:  make(map[string]client),
		remotes:  make(map[string]remote),
		incoming: make(chan clientCommand),
		workers:  1,
		conn:     cn,
	}
	for i := 0; i < ret.workers; i++ {
		go ret.worker()
	}
	go ret.listener()
	return ret
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("%#v", r.Host)
	dat := templateData{
		SocketURL: "ws://" + r.Host + "/socket",
	}

	if temp, err := template.ParseFiles("temp.html"); err != nil {
		log.Printf("%s", err)
	} else if err = temp.Execute(w, dat); err != nil {
		log.Printf("%s", err)
	}
}

func main() {
	log.SetFlags(log.Lshortfile | log.Ldate | log.Ltime)

	addr := ":8080"
	sock := ":13865"
	servers := ""
	help := false

	flag.StringVar(&addr, "addr", ":8080", "server address")
	flag.StringVar(&sock, "sock", ":13865", "socket address")
	flag.StringVar(&servers, "servers", "", "other servers")
	flag.BoolVar(&help, "help", false, "display help message")
	flag.Parse()

	if help {
		flag.Usage()
		os.Exit(1)
	}

	http.HandleFunc("/", indexHandler)
	http.Handle("/socket", NewServer(sock, addr, strings.Split(servers, ",")))
	http.ListenAndServe(addr, nil)
}
