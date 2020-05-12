/*
	Copyright (c) 2019 @crosbymichael

	Permission is hereby granted, free of charge, to any person
	obtaining a copy of this software and associated documentation
	files (the "Software"), to deal in the Software without
	restriction, including without limitation the rights to use, copy,
	modify, merge, publish, distribute, sublicense, and/or sell copies
	of the Software, and to permit persons to whom the Software is
	furnished to do so, subject to the following conditions:

	The above copyright notice and this permission notice shall be
	included in all copies or substantial portions of the Software.

	THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND,
	EXPRESS OR IMPLIED,
	INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
	FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.
	IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT
	HOLDERS BE LIABLE FOR ANY CLAIM,
	DAMAGES OR OTHER LIABILITY,
	WHETHER IN AN ACTION OF CONTRACT,
	TORT OR OTHERWISE,
	ARISING FROM, OUT OF OR IN CONNECTION WITH
	THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	v1 "github.com/crosbymichael/guard/api/v1"
	"github.com/getsentry/sentry-go"
	"github.com/gogo/protobuf/types"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"google.golang.org/grpc"
)

func main() {
	app := cli.NewApp()
	app.Name = "guard"
	app.Version = "1"
	app.Usage = "Wireguard grpc server"
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "debug",
			Usage: "enable debug output in the logs",
		},
		cli.StringFlag{
			Name:  "address,a",
			Usage: "grpc address",
			Value: "10.199.199.1:10100",
		},
		cli.StringFlag{
			Name:   "sentry-dsn",
			Usage:  "sentry DSN",
			EnvVar: "SENTRY_DSN",
		},
	}
	app.Before = func(clix *cli.Context) error {
		if clix.GlobalBool("debug") {
			logrus.SetLevel(logrus.DebugLevel)
		}
		if dsn := clix.GlobalString("sentry-dsn"); dsn != "" {
			err := sentry.Init(sentry.ClientOptions{
				Dsn:     dsn,
				Release: app.Version,
			})
			if err != nil {
				return err
			}
		}
		return nil
	}
	app.Commands = []cli.Command{
		createCommand,
		deleteCommand,
		listCommand,
		serverCommand,
		peersCommand,
	}
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		sentry.CaptureMessage(err.Error())
		if sentry.Flush(time.Second * 2) {
			// event delivered?
		} else {
			fmt.Println(os.Stderr, "Failed to flush error to sentry")
		}
		os.Exit(1)
	}
}

var serverCommand = cli.Command{
	Name:        "server",
	Description: "run the wireguard grpc server",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "dir",
			Usage: "wireguard configuration directory",
			Value: defaultWireguardDir,
		},
		cli.StringFlag{
			Name:  "endpoint",
			Usage: "external endpoint address to manage the wireguard",
		},
	},
	Action: func(clix *cli.Context) error {
		if os.Geteuid() != 0 {
			return errors.New("grpc server must run as root")
		}
		wg, err := newServer(clix.String("dir"))
		if err != nil {
			return err
		}
		server := newGRPC()

		v1.RegisterWireguardServer(server, wg)

		var (
			ctx      = cancelContext()
			endpoint = clix.String("endpoint")
			address  = clix.GlobalString("address")
		)
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return errors.Wrap(err, "splitting tunnel address")
		}
		if endpoint == "" {
			endpoint = net.JoinHostPort("127.0.0.1", port)
		}
		r, err := wg.Create(ctx, &v1.CreateRequest{
			ID:       guardTunnel,
			Address:  host + "/32",
			Endpoint: endpoint,
		})
		if err == nil {
			logrus.Info("created guard0 tunnel")
			if err := jsonTunnel(r.Tunnel); err != nil {
				return err
			}
		} else if err != ErrTunnelExists {
			return err
		}

		// create our server tunnel

		signals := make(chan os.Signal, 32)
		signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			<-signals
			server.Stop()
		}()
		l, err := net.Listen("tcp", address)
		if err != nil {
			return errors.Wrap(err, "listen tcp")
		}
		defer l.Close()
		return server.Serve(l)
	},
}

var createCommand = cli.Command{
	Name:        "create",
	Description: "create a new tunnel",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "address,a",
			Usage: "cidr for the tunnel address",
		},
		cli.StringFlag{
			Name:  "endpoint,e",
			Usage: "set the endpoint for the tunnel",
			Value: "127.0.0.1:31000",
		},
	},
	Action: func(clix *cli.Context) error {
		conn, err := grpc.Dial(clix.GlobalString("address"), grpc.WithInsecure())
		if err != nil {
			return errors.Wrap(err, "dial server")
		}
		defer conn.Close()

		var (
			ctx    = cancelContext()
			client = v1.NewWireguardClient(conn)
		)

		r, err := client.Create(ctx, &v1.CreateRequest{
			ID:       clix.Args().First(),
			Address:  clix.String("address"),
			Endpoint: clix.String("endpoint"),
		})
		if err != nil {
			return err
		}
		return jsonTunnel(r.Tunnel)
	},
}

var listCommand = cli.Command{
	Name:        "list",
	Description: "list all tunnels",
	Action: func(clix *cli.Context) error {
		conn, err := grpc.Dial(clix.GlobalString("address"), grpc.WithInsecure())
		if err != nil {
			return errors.Wrap(err, "dial server")
		}
		defer conn.Close()

		var (
			ctx    = cancelContext()
			client = v1.NewWireguardClient(conn)
		)
		r, err := client.List(ctx, &types.Empty{})
		if err != nil {
			return err
		}
		if len(r.Tunnels) == 0 {
			return nil
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", " ")
		return enc.Encode(r.Tunnels)
	},
}

var peersCommand = cli.Command{
	Name:        "peers",
	Description: "manage peers",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "tunnel,t",
			Usage: "tunnel name",
		},
	},
	Subcommands: []cli.Command{
		{
			Name:        "new",
			Description: "create a new peer",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "ip,i",
					Usage: "ip cidr for the peer",
				},
				cli.StringFlag{
					Name:  "dns",
					Usage: "dns for the peer",
				},
				cli.StringSliceFlag{
					Name:  "ips",
					Usage: "allowed ips",
					Value: &cli.StringSlice{},
				},
			},
			Action: func(clix *cli.Context) error {
				conn, err := grpc.Dial(clix.GlobalString("address"), grpc.WithInsecure())
				if err != nil {
					return errors.Wrap(err, "dial server")
				}
				defer conn.Close()

				var (
					ctx    = cancelContext()
					client = v1.NewWireguardClient(conn)
				)

				r, err := client.NewPeer(ctx, &v1.NewPeerRequest{
					ID:      clix.GlobalString("tunnel"),
					PeerID:  clix.Args().First(),
					Address: clix.String("ip"),
				})
				if err != nil {
					return err
				}
				// generate peer tunnel config
				t := &v1.Tunnel{
					PrivateKey: r.Peer.PrivateKey,
					Address:    r.Peer.AllowedIPs[0],
					DNS:        clix.String("dns"),
					Peers: []*v1.Peer{
						{
							ID:         r.Tunnel.ID,
							PublicKey:  r.Tunnel.PublicKey,
							Endpoint:   net.JoinHostPort(r.Tunnel.Endpoint, r.Tunnel.ListenPort),
							AllowedIPs: clix.StringSlice("ips"),
						},
					},
				}
				return t.Render(os.Stdout)
			},
		},
		{
			Name:        "delete",
			Description: "delete a peer",
			Action: func(clix *cli.Context) error {
				conn, err := grpc.Dial(clix.GlobalString("address"), grpc.WithInsecure())
				if err != nil {
					return errors.Wrap(err, "dial server")
				}
				defer conn.Close()

				var (
					ctx    = cancelContext()
					client = v1.NewWireguardClient(conn)
				)

				r, err := client.DeletePeer(ctx, &v1.DeletePeerRequest{
					ID:     clix.GlobalString("tunnel"),
					PeerID: clix.Args().First(),
				})
				if err != nil {
					return err
				}
				return jsonTunnel(r.Tunnel)
			},
		},
	},
}

var deleteCommand = cli.Command{
	Name:        "delete",
	Description: "delete a tunnel",
	Action: func(clix *cli.Context) error {
		conn, err := grpc.Dial(clix.GlobalString("address"), grpc.WithInsecure())
		if err != nil {
			return errors.Wrap(err, "dial server")
		}
		defer conn.Close()

		var (
			ctx    = cancelContext()
			client = v1.NewWireguardClient(conn)
		)
		if _, err := client.Delete(ctx, &v1.DeleteRequest{
			ID: clix.Args().First(),
		}); err != nil {
			return err
		}
		return nil
	},
}

func newGRPC() *grpc.Server {
	s := grpc.NewServer(
		grpc.UnaryInterceptor(unary),
		grpc.StreamInterceptor(stream),
	)
	return s
}

func unary(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
	r, err := grpc_prometheus.UnaryServerInterceptor(ctx, req, info, handler)
	if err != nil {
		sentry.CaptureException(err)
	}
	return r, err
}

func stream(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	err := grpc_prometheus.StreamServerInterceptor(srv, ss, info, handler)
	if err != nil {
		sentry.CaptureException(err)
	}
	return err
}

func cancelContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	s := make(chan os.Signal)
	signal.Notify(s, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-s
		cancel()
	}()
	return ctx
}

func jsonTunnel(t *v1.Tunnel) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", " ")
	return enc.Encode(t)
}
