package agent

import (
	"net"

	log "github.com/Sirupsen/logrus"

	"github.com/weaveworks/flux/common/daemon"
	"github.com/weaveworks/flux/common/store"
)

type setInstancesConfig struct {
	hostIP net.IP
	store  store.Store

	localInstanceUpdates      <-chan LocalInstanceUpdate
	localInstanceUpdatesReset chan<- struct{}
}

type setInstances struct {
	setInstancesConfig
	errs daemon.ErrorSink
}

func (conf setInstancesConfig) StartFunc() daemon.StartFunc {
	return daemon.SimpleComponent(func(stop <-chan struct{}, errs daemon.ErrorSink) {
		si := setInstances{
			setInstancesConfig: conf,
			errs:               errs,
		}

		si.localInstanceUpdatesReset <- struct{}{}

		for {
			select {
			case update := <-si.localInstanceUpdates:
				si.processUpdate(update)

			case <-stop:
				return
			}
		}
	})
}

func (si *setInstances) processReset(update LocalInstanceUpdate) {
	// We need to get all services, because we need to prune
	// instances on all services, even ones that we no longer have
	// instances for.
	svcs, err := si.store.GetAllServices(store.QueryServiceOptions{WithInstances: true})
	if err != nil {
		si.errs.Post(err)
		return
	}

	for _, svc := range svcs {
		for _, inst := range svc.Instances {
			if !si.hostIP.Equal(inst.Host.IP) {
				continue
			}

			key := InstanceKey{
				Service:  svc.Name,
				Instance: inst.Name,
			}
			if update.LocalInstances[key] == nil {
				si.removeInstance(key)
			}
		}
	}
}

func (si *setInstances) processUpdate(update LocalInstanceUpdate) {
	if update.Reset {
		si.processReset(update)
	}

	for key, inst := range update.LocalInstances {
		if inst == nil {
			si.removeInstance(key)
		} else {
			log.Infof(`Registering service '%s' instance '%.12s' at %s`, key.Service, key.Instance, inst.Address)
			si.errs.Post(si.store.AddInstance(key.Service, key.Instance, *inst))
		}
	}
}

func (si *setInstances) removeInstance(key InstanceKey) {
	log.Infof("Deregistering service '%s' instance '%.12s'", key.Service, key.Instance)
	si.errs.Post(si.store.RemoveInstance(key.Service, key.Instance))
}
