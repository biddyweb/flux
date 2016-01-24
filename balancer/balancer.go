package balancer

import (
	"flag"
	"fmt"
	log "github.com/Sirupsen/logrus"

	"github.com/squaremo/flux/balancer/etcdcontrol"
	"github.com/squaremo/flux/balancer/eventlogger"
	"github.com/squaremo/flux/balancer/events"
	"github.com/squaremo/flux/balancer/model"
	"github.com/squaremo/flux/balancer/prometheus"
	"github.com/squaremo/flux/common/daemon"
	"github.com/squaremo/flux/common/store/etcdstore"
)

func logError(err error, args ...interface{}) {
	if err != nil {
		log.WithError(err).Error(args...)
	}
}

type netConfig struct {
	chain  string
	bridge string
}

type BalancerDaemon struct {
	errorSink   daemon.ErrorSink
	ipTablesCmd IPTablesCmd

	// From flags
	controller   model.Controller
	eventHandler events.Handler
	netConfig    netConfig

	ipTables *ipTables
	services *services
}

func (d *BalancerDaemon) parseArgs(args []string) error {
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)

	var exposePrometheus string
	var debug bool

	// The bridge specified should be the one where packets sent
	// to service IP addresses go.  So even with weave, that's
	// typically 'docker0'.
	fs.StringVar(&d.netConfig.bridge,
		"bridge", "docker0", "bridge device")
	fs.StringVar(&d.netConfig.chain,
		"chain", "FLUX", "iptables chain name")
	fs.StringVar(&exposePrometheus,
		"expose-prometheus", "",
		"expose stats to Prometheus on this IPaddress and port; e.g., :9000")
	fs.BoolVar(&debug, "debug", false, "output debugging logs")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	if fs.NArg() > 0 {
		return fmt.Errorf("excess command line arguments")
	}

	if debug {
		log.SetLevel(log.DebugLevel)
	}
	log.Debug("Debug logging on")

	if exposePrometheus == "" {
		d.eventHandler = eventlogger.EventLogger{}
	} else {
		handler, err := prometheus.NewEventHandler(exposePrometheus)
		if err != nil {
			return err
		}
		d.eventHandler = handler
	}

	store, err := etcdstore.NewFromEnv()
	if err != nil {
		return err
	}

	d.controller, err = etcdcontrol.NewListener(store, d.errorSink)
	if err != nil {
		return err
	}

	return nil
}

func StartBalancer(args []string, errorSink daemon.ErrorSink, ipTablesCmd IPTablesCmd) *BalancerDaemon {
	d := &BalancerDaemon{
		errorSink:   errorSink,
		ipTablesCmd: ipTablesCmd,
	}

	if err := d.parseArgs(args); err != nil {
		errorSink.Post(err)
		return d
	}

	if err := d.start(); err != nil {
		errorSink.Post(err)
	}

	return d
}

func (d *BalancerDaemon) start() error {
	d.ipTables = newIPTables(d.netConfig, d.ipTablesCmd)
	if err := d.ipTables.start(); err != nil {
		return err
	}

	d.services = servicesConfig{
		netConfig:    d.netConfig,
		updates:      d.controller.Updates(),
		eventHandler: d.eventHandler,
		ipTables:     d.ipTables,
		errorSink:    d.errorSink,
	}.start()
	return nil
}

func (d *BalancerDaemon) Stop() {
	if d.controller != nil {
		d.controller.Close()
	}

	if d.services != nil {
		d.services.close()
	}

	if d.ipTables != nil {
		d.ipTables.close()
	}
}
