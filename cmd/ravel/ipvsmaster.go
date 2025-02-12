package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/Comcast/Ravel/pkg/director"
	"github.com/Comcast/Ravel/pkg/iptables"
	"github.com/Comcast/Ravel/pkg/stats"
	"github.com/Comcast/Ravel/pkg/system"
	"github.com/Comcast/Ravel/pkg/util"
	"github.com/Comcast/Ravel/pkg/watcher"
)

// IPVSMASTER runs the ipvs IPVSMASTER - also called ipvs-master
func IPVSMASTER(ctx context.Context, logger logrus.FieldLogger) *cobra.Command {

	var cmd = &cobra.Command{
		Use:           "director",
		Short:         "kube2ipvs director",
		SilenceUsage:  false,
		SilenceErrors: true,
		Long: `
kube2ipvs director will run the kube2ipvs daemon in director mode,
where it will continuously check the kubernetes API for updates to both
node heath as well as the client port configuration.

In director mode, kube2ipvs will directly interact with ipvsadm in order
to delete rules that exist, but no longer apply, and to create rules that
are missing from the configuration.`,
		RunE: func(cmd *cobra.Command, _ []string) error {

			log.Debugln("IPVSMASTER: Starting in DIRECTOR mode")

			config := NewConfig(cmd.Flags())
			logger.Debugf("IPVSMASTER: got config %+v", config)
			b, _ := json.MarshalIndent(config, " ", " ")
			fmt.Println(string(b))

			// validate flags
			logger.Info("IPVSMASTER: validating")
			if err := config.Invalid(); err != nil {
				return err
			}

			// write IPVS Sysctl flags to director node
			log.Debugln("IPVSMASTER: Writing sysctl due to from director startup.")
			if err := config.IPVS.WriteToNode(); err != nil {
				return err
			}

			// instantiate a watcher
			logger.Info("IPVSMASTER: starting watcher")
			watcher, err := watcher.NewWatcher(ctx, config.KubeConfigFile, config.ConfigMapNamespace, config.ConfigMapName, config.ConfigKey, stats.KindIpvsMaster, config.DefaultListener.Service, config.DefaultListener.Port, logger)
			if err != nil {
				return err
			}

			// initialize statistics
			s, err := stats.NewStats(ctx, stats.KindIpvsMaster, config.Stats.Interface, config.Stats.ListenAddr, config.Stats.ListenPort, config.Stats.Interval, logger)
			if err != nil {
				return fmt.Errorf("failed to initialize metrics. %v", err)
			}
			if config.Stats.Enabled {
				if err := s.EnableBPFStats(); err != nil {
					return fmt.Errorf("failed to initialize BPF capture. if=%v sa=%s %v", config.Stats.Interface, config.Stats.ListenAddr, err)
				}
			}
			// emit the version metric
			emitVersionMetric(stats.KindIpvsMaster, config.ConfigMapNamespace, config.ConfigMapName, config.ConfigKey)

			// Starting up control port.
			logger.Infof("IPVSMASTER: starting listen controllers on %v", config.Coordinator.Ports)
			cm := NewCoordinationMetrics(stats.KindIpvsMaster)
			for _, port := range config.Coordinator.Ports {
				go listenController(port, cm, logger)
			}

			// listen for health
			logger.Info("IPVSMASTER: starting health endpoint")
			go util.ListenForHealth(config.Net.Interface, 10201, logger)

			// instantiate a new IPVS manager
			logger.Info("IPVSMASTER: initializing ipvs helper")
			ipvs, err := system.NewIPVS(ctx, config.Net.PrimaryIP, config.IPVS.WeightOverride, config.IPVS.IgnoreCordon, logger, stats.KindIpvsMaster)
			if err != nil {
				return err
			}

			// instantiate an IP helper for loopback and set the arp rules
			// the loopback helper only runs once, at startup
			logger.Info("IPVSMASTER: initializing loopback ip helper")
			ipLoopback, err := system.NewIP(ctx, "lo", config.Net.Gateway, config.Arp.LoAnnounce, config.Arp.LoIgnore, logger)
			if err != nil {
				return err
			}
			if err := ipLoopback.SetARP(); err != nil {
				return err
			}

			// instantiate a new IP helper
			logger.Info("IPVSMASTER: initializing primary ip helper")
			ip, err := system.NewIP(ctx, config.Net.Interface, config.Net.Gateway, config.Arp.PrimaryAnnounce, config.Arp.PrimaryIgnore, logger)
			if err != nil {
				return err
			}

			// instantiate an iptables interface
			logger.Info("IPVSMASTER: initializing iptables")
			ipt, err := iptables.NewIPTables(ctx, stats.KindIpvsMaster, config.ConfigKey, config.PodCIDRMasq, config.IPTablesChain, config.IPTablesMasq, logger)
			if err != nil {
				return err
			}

			// instantiate the director worker.
			logger.Info("IPVSMASTER: initializing director")
			worker, err := director.NewDirector(ctx, config.NodeName, config.ConfigKey, config.CleanupMaster, watcher, ipvs, ip, ipt, config.IPVS.ColocationMode, config.ForcedReconfigure)
			if err != nil {
				return err
			}

			// start the director
			logger.Info("IPVSMASTER: starting worker")
			err = worker.Start()
			if err != nil {
				return err
			}
			logger.Info("IPVSMASTER: started")
			for { // ever
				select {
				case <-ctx.Done():
					// catching exit signals sent from the parent context
					// Removed in VPES-1410. When director exits, we shouldn't clean nup!
					// return worker.Stop()
				}
			}
		},
	}

	cmd.Flags().StringSlice("ipvs-sysctl", []string{""}, "sysctl setting for ipvs. can be passed multiple times. '--ipvs-sysctl=conntrack=0 --ipvs-sysctl=ignore_tunneled=0'")
	viper.BindPFlag("ipvs-sysctl", cmd.Flags().Lookup("ipvs-sysctl"))

	return cmd
}
