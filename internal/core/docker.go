/* docker.go
 *
 * Copyright (C) 2016 Alexandre ACEBEDO
 *
 * This software may be modified and distributed under the terms
 * of the MIT license.  See the LICENSE file for details.
 */

package core

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aacebedo/dnsdock/internal/servers"
	"github.com/aacebedo/dnsdock/internal/utils"
	"github.com/cenkalti/backoff/v4"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// DockerProvider is the name of the provider used for services added by the Docker client
const DockerProvider = "docker"

// DockerManager is the entrypoint to the docker daemon
type DockerManager struct {
	config *utils.Config
	list   servers.ServiceListProvider
	client *client.Client
	cancel context.CancelFunc
}

// NewDockerManager creates a new DockerManager
func NewDockerManager(c *utils.Config, list servers.ServiceListProvider, tlsConfig *tls.Config) (*DockerManager, error) {
	dclient, err := client.NewClientWithOpts(client.WithHost(c.DockerHost), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}

	return &DockerManager{config: c, list: list, client: dclient}, nil
}

// Start starts the DockerManager
func (d *DockerManager) Start() (err error) {
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel

	go func() {
		perr := backoff.RetryNotify(func() error {
			return d.run(ctx)
		}, backoff.WithContext(backoff.NewExponentialBackOff(), ctx), func(err error, d time.Duration) {
			logger.Errorf("[DockerManager] Error running docker manager, retrying in %v: %s", d, err)
		})
		if perr != nil {
			logger.Errorf("[DockerManager] Unrecoverable error running docker manager: %s", perr)
			cancel()
		}
	}()

	return nil
}

func (d *DockerManager) run(ctx context.Context) error {
	messageChan, errorChan := d.client.Events(ctx, types.EventsOptions{
		Filters: filters.NewArgs(filters.Arg("type", "container")),
	})

	logger.Infof("[DockerManager] run at %s", d.config.DockerHost)
	containers, err := d.client.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		logger.Errorf("[DockerManager] Error listing containers: %v", err)
		return fmt.Errorf("error getting containers: %w", err)
	}

	services := make(map[string]struct{})
	for _, container := range containers {
		// Skip containers with host network mode
		containerInfo, err := d.client.ContainerInspect(ctx, container.ID)
		if err != nil {
			logger.Errorf("[DockerManager] Error inspecting container %s: %v", container.ID, err)
		}

		pureName := strings.TrimPrefix(containerInfo.Name,"/")
        logger.Infof("[DockerManager] container name:'%s' with network mode: %s, ID:%s",pureName, containerInfo.HostConfig.NetworkMode,container.ID)
		service, err := d.getService(container.ID)
		if err != nil {
			logger.Errorf("[DockerManager] Error getting service for container %s: %v", container.ID, err)
			return fmt.Errorf("error getting service: %w", err)
		}
		err = d.list.AddService(container.ID, *service)
		if err != nil {
			logger.Errorf("[DockerManager] Error adding service for container %s: %v", container.ID, err)
			return fmt.Errorf("error adding service: %w", err)
		}
		services[container.ID] = struct{}{}
	}

	for id, srv := range d.list.GetAllServices() {
		if _, ok := services[id]; !ok && srv.Provider == DockerProvider {
			err := d.list.RemoveService(id)
			if err != nil {
				// return fmt.Errorf("error removing service: %w", err)
				logger.Errorf("[DockerManager] error removing service: %w", err)
			}
		}
	}

	for {
		select {
		case m := <-messageChan:
			err := d.handler(m,ctx)
			if err != nil {
				logger.Errorf("[DockerManager] run ends for error handling event: %w", err)
				return err
			}
		case err := <-errorChan:
			logger.Errorf("[DockerManager] run ends for error from docker events: %w", err)
			return err
		case <-ctx.Done():
			logger.Infof("[DockerManager] run ends. context done, exiting run loop")
			return nil
		}
	}
}

func (d *DockerManager) handler(m events.Message, ctx context.Context) error {
	if strings.HasPrefix(string(m.Action), "exec_create:") {
		logger.Debugf("[DockerManager] Ignored exec action '%s' for container '%s'", m.Action, m.ID)
		return nil
	}
	if strings.HasPrefix(string(m.Action), "exec_start:") {
		logger.Debugf("[DockerManager] Ignored exec action '%s' for container '%s'", m.Action, m.ID)
		return nil
	}
	if strings.HasPrefix(string(m.Action), "exec_die") {
		logger.Debugf("[DockerManager] Ignored action '%s' for container '%s'", m.Action, m.ID)
		return nil
	}
	if strings.HasPrefix(string(m.Action), "health_status:") {
		logger.Debugf("[DockerManager] Ignored health check action '%s' for container '%s'", m.Action, m.ID)
		return nil
	}

	switch m.Action {
	default:
		containerName := ""
		if name, ok := m.Actor.Attributes["name"]; ok {
			containerName = name
		}
		logger.Infof("[DockerManager] Ignored action: '%s' for container %s: '%s'", m.Action, containerName, m.ID)
		return nil
	case "create":
		return d.createHandler(m)
	case "start":
		return d.startHandler(m)
	case "unpause":
		return d.startHandler(m)
	case "die":
		return d.stopHandler(m)
	case "pause":
		return d.stopHandler(m)
	case "destroy":
		return d.destroyHandler(m)
	case "rename":
		return d.renameHandler(m)
	}
	return nil
}

func (d *DockerManager) createHandler(m events.Message) error {
	containerName := ""
	if name, ok := m.Actor.Attributes["name"]; ok {
		containerName = name
	}
	logger.Infof("[DockerManager] Created container %s: '%s'", containerName, m.ID)
	if d.config.All {
		service, err := d.getService(m.ID)
		if err != nil {
			return fmt.Errorf("error getting service: %w", err)
		}
		err = d.list.AddService(m.ID, *service)
		if err != nil {
			return fmt.Errorf("error adding service: %w", err)
		}
	}
	return nil
}

func (d *DockerManager) startHandler(m events.Message) error {
	containerName := ""
	if name, ok := m.Actor.Attributes["name"]; ok {
		containerName = name
	}
	logger.Infof("[DockerManager] Started container %s: '%s'", containerName, m.ID)
	if !d.config.All {
		service, err := d.getService(m.ID)
		if err != nil {
			return fmt.Errorf("error getting service: %w", err)
		}
		err = d.list.AddService(m.ID, *service)
		if err != nil {
			return fmt.Errorf("error adding service: %w", err)
		}
	}
	return nil
}

func (d *DockerManager) stopHandler(m events.Message) error {
	containerName := ""
	if name, ok := m.Actor.Attributes["name"]; ok {
		containerName = name
	}
	logger.Infof("[DockerManager] Stopped container %s: '%s'", containerName, m.ID)
	if !d.config.All {
		err := d.list.RemoveService(m.ID)
		if err != nil {
			// return fmt.Errorf("error removing service: %w", err)
			logger.Errorf("[DockerManager] error removing service: %w", err)
		}
	} else {
		logger.Infof("[DockerManager] Stopped container %s: '%s' not removed as --all argument is true", containerName, m.ID)
	}
	return nil
}

func (d *DockerManager) renameHandler(m events.Message) error {
	containerName := ""
	if name, ok := m.Actor.Attributes["name"]; ok {
		containerName = name
	}
	logger.Infof("[DockerManager] Renamed container %s: '%s'", containerName, m.ID)
	err := d.list.RemoveService(m.ID)
	if err != nil {
		// return fmt.Errorf("error removing service: %w", err)
		logger.Errorf("[DockerManager] error removing service: %w", err)
	}
	service, err := d.getService(m.ID)
	if err != nil {
		return fmt.Errorf("error getting service: %w", err)
	}
	res := d.list.AddService(m.ID, *service)
	if res != nil {
		// return fmt.Errorf("error removing service: %w", err)
		logger.Errorf("[DockerManager] error removing service: %w", err)
	}
	return nil
}

func (d *DockerManager) destroyHandler(m events.Message) error {
	containerName := ""
	if name, ok := m.Actor.Attributes["name"]; ok {
		containerName = name
	}
	logger.Infof("[DockerManager] Destroy container %s: '%s'", containerName, m.ID)
	if d.config.All {
		err := d.list.RemoveService(m.ID)
		if err != nil {
			// return fmt.Errorf("error removing service: %w", err)
			logger.Errorf("[DockerManager] error removing service: %w", err)
		}
	}
	return nil
}

// Stop stops the DockerManager
func (d *DockerManager) Stop() {
	logger.Infof("[DockerManager] Stopping DockerManager")
	d.cancel()
}
func (d *DockerManager) getTargetContainerIP(networkMode string) ([]net.IP, error) {
	var targetName string
	if strings.HasPrefix(networkMode, "container:") {
		targetName = strings.TrimPrefix(networkMode, "container:")
	} else if strings.HasPrefix(networkMode, "service:") {
		targetName = strings.TrimPrefix(networkMode, "service:")
	} else {
		return nil, fmt.Errorf("unsupported network mode: %s", networkMode)
	}

	containers, err := d.client.ContainerList(context.Background(), container.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, c := range containers {
		info, err := d.client.ContainerInspect(context.Background(), c.ID)
		if err != nil {
			continue
		}
		
		containerName := strings.TrimPrefix(info.Name, "/")
		if containerName == targetName || c.ID == targetName {
			var ips []net.IP
			for _, network := range info.NetworkSettings.Networks {
				if ip := net.ParseIP(network.IPAddress); ip != nil {
					ips = append(ips, ip)
				}
			}
			return ips, nil
		}
	}
	
	return nil, fmt.Errorf("target container not found: %s", targetName)
}

func (d *DockerManager) getService(id string) (*servers.Service, error) {
	desc, err := d.client.ContainerInspect(context.Background(), id)
	if err != nil {
		return nil, err
	}

	service := servers.NewService(DockerProvider)
	service.Aliases = make([]string, 0)

	service.Image = getImageName(desc.Config.Image)
	if imageNameIsSHA(service.Image, desc.Image) {
		logger.Warningf("[DockerManager] Warning: Can't route %s, image %s is not a tag.", id[:10], service.Image)
		service.Image = ""
	}
	service.Name = cleanContainerName(desc.Name)

	// Handle shared network modes
	networkMode := string(desc.HostConfig.NetworkMode)
	logger.Infof("[DockerManager] Processing container '%s' with network mode: '%s'", desc.Name, networkMode)
	if networkMode == "host" {
		logger.Infof("[DockerManager] Detected host network mode for container '%s'", desc.Name)
		hostIP, err := GetHostDockerInternal()
		if err != nil {
			logger.Warningf("[DockerManager] Warning, failed to get host IP for '%s': %v", desc.Name, err)
			return nil, fmt.Errorf("Service '%s' ignored: No IP provided", id)
		}
		logger.Infof("[DockerManager] Assigning host IP %s to container '%s'", hostIP.String(), desc.Name)
		service.IPs = []net.IP{hostIP}
	} else if strings.HasPrefix(networkMode, "container:") || strings.HasPrefix(networkMode, "service:") {
		logger.Infof("[DockerManager] Detected shared network mode for container '%s'", desc.Name)
		ips, err := d.getTargetContainerIP(networkMode)
		if err != nil {
			logger.Warningf("[DockerManager] Warning, failed to get IP from target container for '%s': %v", desc.Name, err)
			return nil, fmt.Errorf("Service '%s' ignored: No IP provided", id)
		}
		service.IPs = ips
	} else {
		// Check if container has network settings
		if len(desc.NetworkSettings.Networks) == 0 {
			// No network settings - container might be sharing another container's network
			// without proper prefix (e.g., Docker Compose format)
			logger.Infof("[DockerManager] No networks found for container '%s', attempting to resolve network mode '%s' as shared network", desc.Name, networkMode)
			ips, err := d.getTargetContainerIP("container:" + networkMode)
			if err == nil && len(ips) > 0 {
				logger.Infof("[DockerManager] Successfully resolved shared network for container '%s'", desc.Name)
				service.IPs = ips
			} else {
				logger.Warningf("[DockerManager] Warning, could not resolve network mode '%s' for container '%s': %v", networkMode, desc.Name, err)
				return nil, fmt.Errorf("Service '%s' ignored: No IP provided", id)
			}
		} else {
			// Normal case - extract IPs from network settings
			for _, value := range desc.NetworkSettings.Networks {
				ip := net.ParseIP(value.IPAddress)
				if ip != nil {
					service.IPs = append(service.IPs, ip)
				}
			}

			if len(service.IPs) == 0 {
				logger.Warningf("[DockerManager] Warning, no IP address found for container '%s'", desc.Name)
				return nil, fmt.Errorf("Service '%s' ignored: No IP provided", id)
			} else {
				logger.Infof("[DockerManager] Found IP addresses for container '%s': %v", desc.Name, service.IPs)
			}
		}
	}

	service = overrideFromLabels(service, desc.Config.Labels)
	service = overrideFromEnv(service, splitEnv(desc.Config.Env))
	if service == nil {
		return nil, errors.New("Skipping " + id)
	}

	if d.config.CreateAlias {
		service.Aliases = append(service.Aliases, service.Name)
	}
	return service, nil
}

func getImageName(tag string) string {
	if index := strings.LastIndex(tag, "/"); index != -1 {
		tag = tag[index+1:]
	}
	if index := strings.LastIndex(tag, ":"); index != -1 {
		tag = tag[:index]
	}
	return tag
}

func imageNameIsSHA(image, sha string) bool {
	// Hard to make a judgement on small image names.
	if len(image) < 4 {
		return false
	}
	// Image name is not HEX
	matched, _ := regexp.MatchString("^[0-9a-f]+$", image)
	if !matched {
		return false
	}
	return strings.HasPrefix(sha, image)
}

func cleanContainerName(name string) string {
	return strings.Replace(name, "/", "", -1)
}

func splitEnv(in []string) (out map[string]string) {
	out = make(map[string]string, len(in))
	for _, exp := range in {
		parts := strings.SplitN(exp, "=", 2)
		var value string
		if len(parts) > 1 {
			value = strings.Trim(parts[1], " ") // trim just in case
		}
		out[strings.Trim(parts[0], " ")] = value
	}
	return
}

func overrideFromLabels(in *servers.Service, labels map[string]string) (out *servers.Service) {
	var region string
	for k, v := range labels {
		if k == "com.dnsdock.ignore" {
			return nil
		}

		if k == "com.dnsdock.alias" {
			in.Aliases = strings.Split(v, ",")
		}

		if k == "com.dnsdock.name" {
			in.Name = v
		}

		if k == "com.dnsdock.tags" {
			if len(v) == 0 {
				in.Name = ""
			} else {
				in.Name = strings.Split(v, ",")[0]
			}
		}

		if k == "com.dnsdock.image" {
			in.Image = v
		}

		if k == "com.dnsdock.ttl" {
			if ttl, err := strconv.Atoi(v); err == nil {
				in.TTL = ttl
			}
		}

		if k == "com.dnsdock.region" {
			region = v
		}

		if k == "com.dnsdock.ip_addr" {
			ipAddr := net.ParseIP(v)
			if ipAddr != nil {
				in.IPs = in.IPs[:0]
				in.IPs = append(in.IPs, ipAddr)
			}
		}

		if k == "com.dnsdock.prefix" {
			addrs := make([]net.IP, 0)
			for _, value := range in.IPs {
				if strings.HasPrefix(value.String(), v) {
					addrs = append(addrs, value)
				}
			}
			if len(addrs) == 0 {
				logger.Warningf("[DockerManager] The prefix '%s' didn't match any IP addresses of service '%s', the service will be ignored", v, in.Name)
			}
			in.IPs = addrs
		}
	}

	if len(region) > 0 {
		in.Image = in.Image + "." + region
	}
	out = in
	return
}

func overrideFromEnv(in *servers.Service, env map[string]string) (out *servers.Service) {
	var region string
	for k, v := range env {
		if k == "DNSDOCK_IGNORE" || k == "SERVICE_IGNORE" {
			return nil
		}

		if k == "DNSDOCK_ALIAS" {
			in.Aliases = strings.Split(v, ",")
		}

		if k == "DNSDOCK_NAME" {
			in.Name = v
		}

		if k == "SERVICE_TAGS" {
			if len(v) == 0 {
				in.Name = ""
			} else {
				in.Name = strings.Split(v, ",")[0]
			}
		}

		if k == "DNSDOCK_IMAGE" || k == "SERVICE_NAME" {
			in.Image = v
		}

		if k == "DNSDOCK_TTL" {
			if ttl, err := strconv.Atoi(v); err == nil {
				in.TTL = ttl
			}
		}

		if k == "SERVICE_REGION" {
			region = v
		}

		if k == "DNSDOCK_IPADDRESS" {
			ipAddr := net.ParseIP(v)
			if ipAddr != nil {
				in.IPs = in.IPs[:0]
				in.IPs = append(in.IPs, ipAddr)
			}
		}

		if k == "DNSDOCK_PREFIX" {
			addrs := make([]net.IP, 0)
			for _, value := range in.IPs {
				if strings.HasPrefix(value.String(), v) {
					addrs = append(addrs, value)
				}
			}
			if len(addrs) == 0 {
				logger.Warningf("[DockerManager] The prefix '%s' didn't match any IP address of  service '%s', the service will be ignored", v, in.Name)
			}
			in.IPs = addrs
		}
	}

	if len(region) > 0 {
		in.Image = in.Image + "." + region
	}
	out = in
	return
}


// GetHostDockerInternal resolves host.docker.internal to get the host's IP address
func GetHostDockerInternal() (net.IP, error) {
	ips, err := net.LookupIP("host.docker.internal")
	if err != nil {
		return nil, fmt.Errorf("failed to resolve host.docker.internal: %w", err)
	}

	// Return the first IPv4 address found
	for _, ip := range ips {
		if ipv4 := ip.To4(); ipv4 != nil {
			return ipv4, nil
		}
	}

	// If no IPv4 found, return the first IP
	if len(ips) > 0 {
		return ips[0], nil
	}

	return nil, fmt.Errorf("no IP addresses found for host.docker.internal")
}
