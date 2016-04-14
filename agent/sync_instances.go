package agent

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/weaveworks/flux/common/daemon"
	"github.com/weaveworks/flux/common/netutil"
	"github.com/weaveworks/flux/common/store"

	log "github.com/Sirupsen/logrus"
	docker "github.com/fsouza/go-dockerclient"
)

type instanceSet map[string]struct{}

const (
	GLOBAL = "global"
	LOCAL  = "local"
)

func IsValidNetworkMode(mode string) bool {
	return mode == GLOBAL || mode == LOCAL
}

type service struct {
	*store.ServiceInfo
	localInstances instanceSet
}

func (svc *service) includes(instanceName string) bool {
	_, ok := svc.localInstances[instanceName]
	return ok
}

type syncInstancesConfig struct {
	hostIP  net.IP
	network string
	store   store.Store

	containerUpdates      <-chan ContainerUpdate
	containerUpdatesReset chan<- struct{}
	serviceUpdates        <-chan store.ServiceUpdate
	serviceUpdatesReset   chan<- struct{}
}

type syncInstances struct {
	syncInstancesConfig
	errs       daemon.ErrorSink
	services   map[string]*service
	containers map[string]*docker.Container
}

func (conf syncInstancesConfig) StartFunc() daemon.StartFunc {
	return daemon.SimpleComponent(func(stop <-chan struct{}, errs daemon.ErrorSink) {
		si := syncInstances{
			syncInstancesConfig: conf,
			errs:                errs,
		}

		si.containerUpdatesReset <- struct{}{}
		si.serviceUpdatesReset <- struct{}{}

		for {
			select {
			case update := <-si.containerUpdates:
				si.processContainerUpdate(update)

			case update := <-si.serviceUpdates:
				si.processServiceUpdate(update)

			case <-stop:
				return
			}
		}
	})
}

func (si *syncInstances) processContainerUpdate(update ContainerUpdate) {
	if update.Reset {
		si.containers = update.Containers
		for _, svc := range si.services {
			si.errs.Post(si.syncInstances(svc))
		}

		return
	}

	for id, cont := range update.Containers {
		if cont != nil {
			si.containers[id] = cont
			si.errs.Post(si.addContainer(cont))
		} else if cont := si.containers[id]; cont != nil {
			delete(si.containers, id)
			si.errs.Post(si.removeContainer(cont))
		}
	}
}

func (si *syncInstances) addContainer(container *docker.Container) error {
	for _, service := range si.services {
		log.Infof(`Evaluating container '%s' against service '%s'`, container.ID, service.Name)
		if err := si.evaluate(container, service); err != nil {
			return err
		}
	}
	return nil
}

func (si *syncInstances) removeContainer(container *docker.Container) error {
	instName := instanceNameFor(container)
	for serviceName, svc := range si.services {
		if svc.includes(instName) {
			err := si.store.RemoveInstance(serviceName, instName)
			if err != nil {
				return err
			}
			log.Infof("Deregistered service '%s' instance '%.12s'", serviceName, instName)
			delete(svc.localInstances, instName)
		}
	}
	return nil
}

func (si *syncInstances) processServiceUpdate(update store.ServiceUpdate) {
	if update.Reset {
		si.services = make(map[string]*service)
	}

	for name, svcInfo := range update.Services {
		if svcInfo != nil {
			svc := si.redefineService(svcInfo)
			si.errs.Post(si.syncInstances(svc))
		} else if svc := si.containers[name]; svc != nil {
			delete(si.services, name)
		}
	}
}

// The service has been changed; re-evaluate which containers belong,
// and which don't. Assume we have a correct list of containers.
func (si *syncInstances) redefineService(svcInfo *store.ServiceInfo) *service {
	svc, found := si.services[svcInfo.Name]
	if !found {
		svc = &service{}
		si.services[svcInfo.Name] = svc
	}
	svc.ServiceInfo = svcInfo
	return svc
}

func (si *syncInstances) syncInstances(svc *service) error {
	if si.containers == nil {
		// Defer syncing instances until we learn about containers
		return nil
	}

	svc.localInstances = make(instanceSet)
	for _, container := range si.containers {
		if err := si.evaluate(container, svc); err != nil {
			return err
		}
	}

	// remove any instances for this service that do not match
	storeSvc, err := si.store.GetService(svc.Name, store.QueryServiceOptions{WithInstances: true})
	if err != nil {
		return err
	}

	for _, inst := range storeSvc.Instances {
		if !svc.includes(inst.Name) && si.owns(inst.Instance) {
			if err := si.store.RemoveInstance(svc.Name, inst.Name); err != nil {
				return err
			}
		}
	}

	return nil
}

func (si *syncInstances) owns(inst store.Instance) bool {
	return si.hostIP.Equal(inst.Host.IP)
}

func (si *syncInstances) evaluate(container *docker.Container, svc *service) error {
	for _, spec := range svc.ContainerRules {
		if instance, ok := si.extractInstance(spec.ContainerRule, svc.ServiceInfo.Service, container); ok {
			instance.ContainerRule = spec.Name
			instName := instanceNameFor(container)
			err := si.store.AddInstance(svc.Name, instName, instance)
			if err != nil {
				return err
			}
			svc.localInstances[instName] = struct{}{}
			log.Infof(`Registered %s instance '%.12s' at %s`, svc.Name, instName, instance.Address)
			return nil
		}
	}
	return nil
}

// instanceNameFor and instanceNameFromEvent encode the fact we just
// use the container ID as the instance name.
func instanceNameFor(c *docker.Container) string {
	return c.ID
}

func (si *syncInstances) extractInstance(spec store.ContainerRule, svc store.Service, container *docker.Container) (store.Instance, bool) {
	var inst store.Instance
	if !spec.Includes(containerLabels{container}) {
		return inst, false
	}

	inst.Address = si.getAddress(spec, svc, container)
	if inst.Address == nil {
		log.Infof(`Cannot extract address for instance, from container '%s'`, container.ID)
	}

	labels := map[string]string{
		"tag":   imageTag(container.Config.Image),
		"image": imageName(container.Config.Image),
	}
	for k, v := range container.Config.Labels {
		labels[k] = v
	}
	for _, v := range container.Config.Env {
		kv := strings.SplitN(v, "=", 2)
		labels["env."+kv[0]] = kv[1]
	}
	inst.Labels = labels
	inst.Host = store.Host{IP: si.hostIP}
	return inst, true
}

type containerLabels struct{ *docker.Container }

func (container containerLabels) Label(label string) string {
	switch {
	case label == "image":
		return imageName(container.Config.Image)
	case label == "tag":
		return imageTag(container.Config.Image)
	case len(label) > 4 && label[:4] == "env.":
		return envValue(container.Config.Env, label[4:])
	default:
		return container.Config.Labels[label]
	}
}

/*
Extract an address from a container, according to what we've been told
about the service.

There are two special cases:
 - if the service has no instance port, we have no chance of getting an
address, so just let the container be considered unaddressable;
 - if the container has been run with `--net=host`; this means the
container is using the host's networking stack, so we should use the
host IP address.

*/
func (si *syncInstances) getAddress(spec store.ContainerRule, svc store.Service, container *docker.Container) *netutil.IPPort {
	if svc.InstancePort == 0 {
		return nil
	}
	if container.HostConfig.NetworkMode == "host" {
		return &netutil.IPPort{si.hostIP, svc.InstancePort}
	}
	switch si.network {
	case LOCAL:
		return si.mappedPortAddress(container, svc.InstancePort)
	case GLOBAL:
		return si.fixedPortAddress(container, svc.InstancePort)
	}
	return nil
}

/*
Extract a "mapped port" address. This mode assumes the balancer is
connecting to containers via a port "mapped" (NATed) by
Docker. Therefore it looks for the port mentioned in the list of
published ports, and finds the host port it has been mapped to. The IP
address is that given as the host's IP address.
*/
func (si *syncInstances) mappedPortAddress(container *docker.Container, port int) *netutil.IPPort {
	p := docker.Port(fmt.Sprintf("%d/tcp", port))
	if bindings, found := container.NetworkSettings.Ports[p]; found {
		for _, binding := range bindings {
			switch binding.HostIP {
			case "", "0.0.0.0":
				// matches
			default:
				ip := net.ParseIP(binding.HostIP)
				if ip == nil || !ip.Equal(si.hostIP) {
					continue
				}
			}

			mappedToPort, err := strconv.Atoi(binding.HostPort)
			if err != nil {
				return nil
			}

			return &netutil.IPPort{si.hostIP, mappedToPort}
		}
	}

	return nil
}

/*
Extract a "fixed port" address. This mode assumes that the balancer
will be able to connect to the container, potentially across hosts,
using the address Docker has assigned it.
*/
func (si *syncInstances) fixedPortAddress(container *docker.Container, port int) *netutil.IPPort {
	ip := net.ParseIP(container.NetworkSettings.IPAddress)
	if ip == nil {
		return nil
	}

	return &netutil.IPPort{ip, port}
}

func envValue(env []string, key string) string {
	for _, entry := range env {
		keyval := strings.Split(entry, "=")
		if keyval[0] == key {
			return keyval[1]
		}
	}
	return ""
}

func (si *syncInstances) processHostChange(change store.HostChange) {
	// TODO: if the host has been removed, mark the instances as dubious, and schedule something to delete them if that's still the case in now + T.
	action := "arrived"
	if change.HostDeparted {
		action = "departed"
	}
	log.Infof("Host change: %s %s", change.Name, action)
}

func imageTag(image string) string {
	colon := strings.LastIndex(image, ":")
	if colon == -1 {
		return "latest"
	}
	return image[colon+1:]
}

func imageName(image string) string {
	colon := strings.LastIndex(image, ":")
	if colon == -1 {
		return image
	}
	return image[:colon]
}
