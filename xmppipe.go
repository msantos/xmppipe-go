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
	"crypto/rand"
	"crypto/tls"
	"encoding/base32"
	"flag"
	"fmt"
	"github.com/mattn/go-xmpp"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

type stdio struct {
	buf string
	err error
}

var server = flag.String("server", "", "server")
var username = flag.String("username", "", "username")
var password = flag.String("password", "", "password")
var status = flag.String("status", "xa", "status")
var statusMessage = flag.String("status-msg", "stdin", "status message")
var stdout = flag.String("stdout", "", "XMPP MUC (multiuser chatroom)")
var nick = flag.String("nick", "xmppipe", "nickname")
var notls = flag.Bool("notls", true, "No TLS")
var debug = flag.Bool("debug", false, "debug output")
var eof = flag.Bool("eof", true, "Exit on EOF")
var session = flag.Bool("session", true, "use server session")
var noverify = flag.Bool("noverify", false, "verify server SSL certificate")
var sigpipe = flag.Bool("sigpipe", false, "Exit when stdout closes (no occupants in MUC)")
var discard = flag.Bool("discard", false, "Discard stdout when no occupants")
var keepalive = flag.Int("keepalive", 60000, "Keepalive sent after inactivity (ms)")

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: xmppipe [options]\n")
		flag.PrintDefaults()
		os.Exit(2)
	}
	flag.Parse()
	if *username == "" || *password == "" {
		*username, *password = xmpp_account()
		if *username == "" || *password == "" {
			flag.Usage()
		}
	}

	servers := xmpplookup(*username, *server)

	if *stdout == "" {
		*stdout = roomname()
		fmt.Fprintf(os.Stderr, "room:%s\n", *stdout)
	}

	xmpp.DefaultConfig = tls.Config{
		ServerName:         serverName(*server),
		InsecureSkipVerify: *noverify,
	}

	var talk *xmpp.Client
	var err error

	for _, host := range servers {
		options := xmpp.Options{
			Host:          host,
			User:          *username,
			Password:      *password,
			NoTLS:         *notls,
			Debug:         *debug,
			Session:       *session,
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

	if err != nil {
		log.Fatal(err)
	}

	talk.JoinMUC(*stdout, *nick)

	var occupants int
	signal := open_stdout(talk)
	stdin := open_stdin()

	for {
		select {
		case v := <-signal:
			if v.Type == "" {
				occupants += 1
			} else {
				if occupants > 0 {
					occupants -= 1
				}
			}
			if *sigpipe && occupants == 0 {
				os.Exit(0)
			}
		case in := <-stdin:
			if in.err == io.EOF {
				if *eof {
					talk.Close()
					os.Exit(0)
				} else {
					continue
				}
			}
			if in.err != nil {
				log.Fatal(in.err)
			}
			if *discard && occupants == 0 {
				continue
			}
			if *sigpipe && occupants == 0 {
				os.Exit(0)
			}
			_, err = talk.Send(xmpp.Chat{Remote: *stdout, Type: "groupchat", Text: in.buf})
			if err != nil {
				log.Fatal(err)
			}
		case <-time.After(time.Duration(*keepalive) * time.Millisecond):
			_, err = talk.SendOrg(" ")
			if err != nil {
				log.Fatal(err)
			}
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

func open_stdout(talk *xmpp.Client) chan xmpp.Presence {
	signal := make(chan xmpp.Presence)

	go func() {
		jid := fmt.Sprintf("%s/%s", *stdout, *nick)

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
				if strings.HasPrefix(v.From, *stdout) && v.From != jid {
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

func xmpp_account() (username, password string) {
	return os.Getenv("XMPPIPE_USERNAME"), os.Getenv("XMPPIPE_PASSWORD")
}

func roomname() string {
	tokens := strings.SplitN(*username, "@", 2)
	id := make([]byte, 5)
	_, err := rand.Read(id)
	if err != nil {
		log.Fatal(err)
	}
	return fmt.Sprintf("stdout-%s@conference.%s",
		strings.ToLower(base32.StdEncoding.EncodeToString(id)),
		tokens[1])
}

func xmpplookup(jid, server string) []string {
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
