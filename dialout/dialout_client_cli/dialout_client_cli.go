// The dialout_client_cli program implements the telemetry publish client.
package main

import (
	"crypto/tls"
	"flag"
	dc "github.com/Azure/sonic-telemetry/dialout/dialout_client"
	"github.com/golang/glog"
	gpb "github.com/openconfig/gnmi/proto/gnmi"
	"golang.org/x/net/context"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"time"
)

var (
	clientCfg = dc.ClientConfig{
		SrcIp:          "",
		RetryInterval:  30 * time.Second,
		Encoding:       gpb.Encoding_JSON_IETF,
		Unidirectional: true,
		TLS:            &tls.Config{},
	}
	pprof int    // pprof enabled
)

func init() {
	flag.IntVar(&pprof, "pprof", 0, "enable pprof")
	flag.StringVar(&clientCfg.TLS.ServerName, "server_name", "", "When set, use this hostname to verify server certificate during TLS handshake.")
	flag.BoolVar(&clientCfg.TLS.InsecureSkipVerify, "insecure", false, "When set, client will not verify the server certificate during TLS handshake.")
	flag.DurationVar(&clientCfg.RetryInterval, "retry_interval", 30*time.Second, "Interval at which client tries to reconnect to destination servers")
	flag.BoolVar(&clientCfg.Unidirectional, "unidirectional", true, "No repesponse from server is expected")
}

func main() {
	flag.Parse()

	if pprof != 0 {
		go func() {
			glog.Info(http.ListenAndServe(":6060", nil))
		}()
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Terminate on Ctrl+C
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
		cancel()
	}()

	glog.V(1).Infof("Starting telemetry publish client")
	err := dc.DialOutRun(ctx, &clientCfg)
	glog.V(1).Infof("Exiting telemetry publish client: %v", err)
	glog.Flush()
}
