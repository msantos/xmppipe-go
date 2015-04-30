// Copyright (c) 2015, Michael Santos <michael.santos@gmail.com>
// Permission to use, copy, modify, and/or distribute this software for any
// purpose with or without fee is hereby granted, provided that the above
// copyright notice and this permission notice appear in all copies.
//
// THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
// WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
// ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
// WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
// ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
// OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	"github.com/mattn/go-xmpp"
	"io"
	"log"
	"net"
	"os"
	"runtime"
	"strings"
	"time"
)

var version = "0.1.0"

type stdio struct {
	buf string
	err error
}

var server = flag.String("server", "", "server")
var username = flag.String("username", "", "username")
var password = flag.String("password", "", "password")
var status = flag.String("status", "xa", "status")
var statusMessage = flag.String("status-msg", "stdin", "status message")
var subject = flag.String("subject", "", "MUC subject")
var stdout = flag.String("stdout", "", "XMPP MUC (multiuser chatroom)")
var resource = flag.String("resource", "xmppipe", "resource")
var usetls = flag.Bool("tls", false, "Use TLS")
var debug = flag.Bool("debug", false, "enable debug output")
var noeof = flag.Bool("noeof", false, "Don't exit when stdin is closed")
var nosession = flag.Bool("nosession", false, "disable use of server session")
var noverify = flag.Bool("noverify", false, "verify server SSL certificate")
var sigpipe = flag.Bool("sigpipe", false, "Exit when stdout closes (no occupants in MUC)")
var discard = flag.Bool("discard", false, "Discard stdout when no occupants")
var keepalive = flag.Int("keepalive", 60, "Keepalive sent after inactivity (seconds)")
var maxline = flag.Int("maxline", 10, "Number of lines to buffer before sending")

func getenv(key *string, env string) {
	if *key == "" {
		*key = os.Getenv(env)
	}
	if *key == "" {
		usage()
	}
}

func usage() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: xmppipe [options] (version: %s)\n",
			version)
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()
}

func main() {
	usage()

	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(runtime.NumCPU())
	}

	getenv(username, "XMPPIPE_USERNAME")
	getenv(password, "XMPPIPE_PASSWORD")

	if *maxline < 1 {
		*maxline = 1
	}

	servers := xmpp_lookup(*username, *server)

	talk, err := xmpp_connect(servers)

	if err != nil {
		log.Fatal(err)
	}
	defer talk.Close()

	if *stdout == "" {
		*stdout = roomname()
		fmt.Fprintf(os.Stderr, "room:%s\n", *stdout)
	}

	talk.JoinMUC(*stdout, *resource)

	if *subject != "" {
		_, err = xmpp_subject(talk, *subject)
		if err != nil {
			log.Fatal(err)
		}
	}

	var occupants int
	jid := fmt.Sprintf("%s/%s", *stdout, *resource)

	signal := open_stdout(talk, jid)
	xmpp_waitjoin(signal, jid, &occupants)

	eof := make(chan bool)
	msg := xmpp_send(talk, *maxline, eof)
	stdin := open_stdin()

	for {
		select {
		case v := <-signal:
			xmpp_roomcount(v, &occupants)
			if *sigpipe && occupants == 0 {
				goto EOF
			}
		case in := <-stdin:
			switch {
			case *noeof && in.err == io.EOF:
				continue
			case in.err == io.EOF:
				goto EOF
			case in.err != nil:
				log.Fatal(in.err)
			}
			if *discard && occupants == 0 {
				continue
			}
			if *sigpipe && occupants == 0 {
				goto EOF
			}
			msg <- in.buf
			continue
		case <-time.After(time.Duration(*keepalive) * time.Second):
			_, err = talk.SendOrg(" ")
			if err != nil {
				log.Fatal(err)
			}
		}
	}

EOF:
	eof <- true
	<-eof
}

func xmpp_roomcount(v xmpp.Presence, occupants *int) {
	if v.Type == "" {
		*occupants += 1
	} else {
		if *occupants > 0 {
			*occupants -= 1
		}
	}
}

func xmpp_connect(servers []string) (*xmpp.Client, error) {
	var talk *xmpp.Client
	var err error

	xmpp.DefaultConfig = tls.Config{
		ServerName:         serverName(*server),
		InsecureSkipVerify: *noverify,
	}

	for _, host := range servers {
		options := xmpp.Options{
			Host:          host,
			User:          *username,
			Password:      *password,
			NoTLS:         !*usetls,
			Debug:         *debug,
			Session:       !*nosession,
			Status:        *status,
			StatusMessage: *statusMessage,
		}

		talk, err = options.NewClient()

		if err == nil {
			break
		}

		if *debug {
			fmt.Fprintln(os.Stderr, host, err)
		}
	}

	return talk, err
}

func xmpp_waitjoin(signal chan xmpp.Presence, jid string, occupants *int) {
	for {
		select {
		case v := <-signal:
			if v.From == jid {
				return
			}
			xmpp_roomcount(v, occupants)
		}
	}
}

func open_stdin() chan stdio {
	stdin := make(chan stdio)

	go func() {
		var in stdio
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			in.buf = scanner.Text()
			stdin <- in
		}
		in.err = scanner.Err()
		if in.err == nil {
			in.err = io.EOF
		}
		stdin <- in
	}()

	return stdin
}

func open_stdout(talk *xmpp.Client, jid string) chan xmpp.Presence {
	signal := make(chan xmpp.Presence)

	go func() {
		for {
			chat, err := talk.Recv()
			if err != nil {
				log.Fatal(err)
			}
			switch v := chat.(type) {
			case xmpp.Chat:
				if v.Remote != jid {
					fmt.Printf("message:%s:%v:%s\n", v.Type, v.Remote, v.Text)
				}
				if *debug {
					fmt.Fprintf(os.Stderr, "%+v\n", v)
				}
			case xmpp.Presence:
				if strings.HasPrefix(v.From, *stdout) {
					ptype := v.Type
					if ptype == "" {
						ptype = "available"
					}
					signal <- v
					fmt.Printf("presence:%s:%s:%s:%s\n", ptype, v.From, v.To, v.Show)
				}
				if *debug {
					fmt.Fprintf(os.Stderr, "%+v\n", v)
				}
			}
		}
	}()

	return signal
}

func xmpp_send(talk *xmpp.Client, bufsz int, eof chan bool) chan string {
	msg := make(chan string)
	year := time.Hour * 24 * 365

	go func() {
		var buf []string
		var flush = false
		duration := year

		for {
			select {
			case m := <-msg:
				if len(buf) == 0 {
					duration = time.Duration(1000) * time.Millisecond
				}
				buf = append(buf, m)
				if len(buf) < bufsz {
					continue
				}
			case flush = <-eof:
			case <-time.After(duration):
			}
			_, err := talk.Send(xmpp.Chat{Remote: *stdout, Type: "groupchat", Text: strings.Join(buf, "\n")})
			if err != nil {
				log.Fatal(err)
			}
			buf = []string{}
			duration = year

			if flush {
				goto EOF
			}
		}
	EOF:
		eof <- true
	}()

	return msg
}

func xmpp_subject(talk *xmpp.Client, subject string) (int, error) {
	stanza := fmt.Sprintf("<message to='%s' type='%s'>"+
		"<subject>%s</subject></message>",
		xmlEscape(*stdout), "groupchat", xmlEscape(subject))
	return talk.SendOrg(stanza)
}

func roomname() string {
	tokens := strings.SplitN(*username, "@", 2)
	name, err := os.Hostname()
	if err != nil {
		name = "nohost"
	}
	return fmt.Sprintf("stdout-%s-%d@conference.%s",
		name,
		os.Getpid(),
		tokens[1])
}

func xmpp_lookup(jid, server string) []string {
	if server != "" {
		return []string{server}
	}

	var servers []string

	host := strings.Split(jid, "@")[1]
	_, addrs, _ := net.LookupSRV("xmpp-client", "tcp", host)
	for _, srv := range addrs {
		servers = append(servers, fmt.Sprintf("%s:%d", srv.Target, srv.Port))
	}

	return append(servers, fmt.Sprintf("%s:5222", host))
}

func serverName(host string) string {
	return strings.Split(host, ":")[0]
}

var xmlSpecial = map[byte]string{
	'<':  "&lt;",
	'>':  "&gt;",
	'"':  "&quot;",
	'\'': "&apos;",
	'&':  "&amp;",
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		c := s[i]
		if s, ok := xmlSpecial[c]; ok {
			b.WriteString(s)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}
