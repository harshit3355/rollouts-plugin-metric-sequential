// Command rollouts-plugin-metric-sequential is an Argo Rollouts metric
// provider plugin. The Rollouts controller starts it and talks to it over
// go-plugin RPC.
package main

import (
	"time"

	rolloutsPlugin "github.com/argoproj/argo-rollouts/metricproviders/plugin/rpc"
	goPlugin "github.com/hashicorp/go-plugin"
	log "github.com/sirupsen/logrus"

	"github.com/harshit3355/rollouts-plugin-metric-sequential/internal/plugin"
)

// The handshake values are fixed by Argo Rollouts for metric provider plugins.
var handshakeConfig = goPlugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "ARGO_ROLLOUTS_RPC_PLUGIN",
	MagicCookieValue: "metricprovider",
}

func main() {
	impl := &plugin.RpcPlugin{
		LogCtx:     *log.WithFields(log.Fields{"plugin": plugin.Name}),
		NewQuerier: plugin.NewPrometheusQuerier,
		Now:        time.Now,
	}
	goPlugin.Serve(&goPlugin.ServeConfig{
		HandshakeConfig: handshakeConfig,
		Plugins: map[string]goPlugin.Plugin{
			"RpcMetricProviderPlugin": &rolloutsPlugin.RpcMetricProviderPlugin{Impl: impl},
		},
	})
}
