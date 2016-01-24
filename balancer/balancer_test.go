package balancer

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/squaremo/flux/balancer/eventlogger"
	"github.com/squaremo/flux/balancer/model"
	"github.com/squaremo/flux/common/daemon"
)

func TestBalancer(t *testing.T) {
	ipTables := newMockIPTables(t)
	d := BalancerDaemon{
		errorSink:    daemon.NewErrorSink(),
		ipTablesCmd:  ipTables.cmd,
		controller:   mockController{},
		eventHandler: eventlogger.EventLogger{},
		netConfig: netConfig{
			chain:  "FLUX",
			bridge: "docker0",
		},
	}

	require.Nil(t, d.start())
	d.Stop()

	// check that iptables was cleaned up
	for c, _ := range ipTables.chains {
		require.Contains(t, builtinChains, c)
	}
}

type mockController struct{}

func (mockController) Updates() <-chan model.ServiceUpdate {
	return nil
}

func (mockController) Close() {
}
