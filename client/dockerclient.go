/*
###########################################################################
#
#   Filename:           dockerclient.go
#
#   Author:             Siva Teja Areti
#   Created:            June 6, 2017
#
#   Description:        libnetwork docker client API
#
###########################################################################
#
#              Copyright (c) 2017 Nuage Networks
#
###########################################################################
*/

package client

import (
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/swarm"
	dockerClient "github.com/docker/docker/client"
	nuageApi "github.com/nuagenetworks/nuage-libnetwork/api"
	nuageConfig "github.com/nuagenetworks/nuage-libnetwork/config"
	"github.com/nuagenetworks/nuage-libnetwork/utils"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

//NuageDockerClient structure holds docker client
type NuageDockerClient struct {
	socketFile         string
	dclient            *dockerClient.Client
	connectionRetry    chan bool
	connectionActive   chan bool
	stop               chan bool
	dockerChannel      chan *nuageApi.DockerEvent
	vsdChannel         chan *nuageApi.VSDEvent
	networkParamsTable *utils.HashMap
	serviceIPCache     *utils.HashMap
	pluginVersion      string
	sync.Mutex
}

//NewNuageDockerClient creates a new docker client
func NewNuageDockerClient(config *nuageConfig.NuageLibNetworkConfig, channels *nuageApi.NuageLibNetworkChannels) (*NuageDockerClient, error) {
	var err error
	nuagedocker := &NuageDockerClient{}
	nuagedocker.stop = channels.Stop
	nuagedocker.dockerChannel = channels.DockerChannel
	nuagedocker.vsdChannel = channels.VSDChannel
	nuagedocker.connectionRetry = make(chan bool)
	nuagedocker.connectionActive = make(chan bool)
	nuagedocker.networkParamsTable = utils.NewHashMap()
	nuagedocker.serviceIPCache = utils.NewHashMap()
	nuagedocker.pluginVersion = config.PluginVersion
	nuagedocker.socketFile = config.DockerSocketFile
	nuagedocker.dclient, err = connectToDockerDaemon(nuagedocker.socketFile)
	if err != nil {
		log.Errorf("Connecting to docker client failed with error %v", err)
		return nil, err
	}
	log.Debugf("Finished initializing docker module")
	return nuagedocker, nil
}

//GetRunningContainerList fetches the list of running containers from docker
func (nuagedocker *NuageDockerClient) GetRunningContainerList() ([]types.Container, error) {
	var activeContainersList []types.Container
	var err error
	nuagedocker.executeDockerCommand(
		func() error {
			activeContainersList, err = nuagedocker.dclient.ContainerList(context.Background(), types.ContainerListOptions{})
			return err
		})
	if err != nil {
		log.Errorf("Getting list of running containers failed with error: %v", err)
	}
	log.Debugf("number of containers in docker ps = %d", len(activeContainersList))
	return activeContainersList, nil
}

//CheckNetworkList checks if the given params matches existing network params
func (nuagedocker *NuageDockerClient) CheckNetworkList(nuageParams *nuageConfig.NuageNetworkParams) (bool, error) {
	networkList, err := nuagedocker.dockerNetworkList()
	if err != nil {
		log.Errorf("Retrieving existing networks from docker failed with error: %v", err)
		return true, err
	}

	_, newSubnet, err := net.ParseCIDR(nuageParams.SubnetCIDR)
	if err != nil {
		log.Errorf("ParseCIDR failed for address %s with error: %v", nuageParams.SubnetCIDR, err)
		return true, err
	}
	for _, network := range networkList {
		existingNetworkOptions := nuageConfig.ParseNuageParams(network.IPAM.Options)
		matchingNetworkOpts := nuageConfig.IsSameNetworkOpts(existingNetworkOptions, nuageParams)

		var overlappingSubnets bool
		for _, nwConfig := range network.IPAM.Config {
			_, existingSubnet, err := net.ParseCIDR(nwConfig.Subnet)
			if err != nil {
				log.Errorf("ParseCIDR failed for address %s with error: %v", nwConfig.Subnet, err)
				return true, err
			}
			if newSubnet.Contains(existingSubnet.IP) || existingSubnet.Contains(newSubnet.IP) {
				overlappingSubnets = true
			}
		}

		if matchingNetworkOpts && overlappingSubnets {
			return true, fmt.Errorf("Network options and subnet overlap with existing network")
		}
	}

	return false, nil
}

//GetNetworkOptsFromPoolID fetches network options for a given docker network
func (nuagedocker *NuageDockerClient) GetNetworkOptsFromPoolID(poolID string) (*nuageConfig.NuageNetworkParams, error) {
	networkOpts := &nuageConfig.NuageNetworkParams{}
	networkList, err := nuagedocker.dockerNetworkList()
	if err != nil {
		log.Errorf("Retrieving existing networks from docker failed with error: %v", err)
		return nil, err
	}
	for _, network := range networkList {
		if network.IPAM.Options == nil || len(network.IPAM.Config) == 0 {
			continue
		}
		networkOpts = nuageConfig.ParseNuageParams(network.IPAM.Options)
		networkOpts.SubnetCIDR = network.IPAM.Config[0].Subnet
		if poolID == nuageConfig.MD5Hash(networkOpts) {
			return networkOpts, nil
		}
	}
	return nil, fmt.Errorf("network options with matching poolID not found")
}

//GetNetworkOptsFromNetworkID fetches a network from docker
func (nuagedocker *NuageDockerClient) GetNetworkOptsFromNetworkID(networkID string) (*nuageConfig.NuageNetworkParams, error) {
	var networkInspect types.NetworkResource
	var err error

	nuagedocker.executeDockerCommand(
		func() error {
			networkInspect, err = nuagedocker.dclient.NetworkInspect(context.Background(), networkID, types.NetworkInspectOptions{})
			return err
		})
	if err != nil {
		log.Errorf("Retrieving existing networks from docker failed with error: %v", err)
		return nil, err
	}

	if networkInspect.IPAM.Options == nil || len(networkInspect.IPAM.Config) == 0 {
		return nil, fmt.Errorf("error reading network %s information from docker", networkID)
	}

	networkParams := nuageConfig.ParseNuageParams(networkInspect.IPAM.Options)
	networkParams.SubnetCIDR = networkInspect.IPAM.Config[0].Subnet
	networkParams.Gateway = networkInspect.IPAM.Config[0].Gateway

	nuagedocker.networkParamsTable.Write(networkID, networkParams)

	return networkParams, nil
}

//GetContainerInspect returns the container inspect output of a container
func (nuagedocker *NuageDockerClient) GetContainerInspect(uuid string) (types.ContainerJSON, error) {
	var containerInspect types.ContainerJSON
	var err error

	nuagedocker.executeDockerCommand(
		func() error {
			containerInspect, err = nuagedocker.dclient.ContainerInspect(context.Background(), uuid)
			return err
		})
	if err != nil {
		log.Errorf("Inspect on container %s failed with error %v", uuid, err)
		return types.ContainerJSON{}, err
	}

	return containerInspect, nil
}

//GetNetworkConnectEvents listens for event when a container is connected to "nuage" network
func (nuagedocker *NuageDockerClient) GetNetworkConnectEvents() {
	filterArgs := filters.NewArgs()
	filterArgs.Add("type", "network")
	filterArgs.Add("event", "connect")
	options := types.EventsOptions{
		Filters: filterArgs,
	}

	eventsChanRO, errChan := nuagedocker.dclient.Events(context.Background(), options)
	for {
		select {
		case eventMsg := <-eventsChanRO:
			if eventMsg.Actor.Attributes["type"] == nuageConfig.DockerNetworkType[nuagedocker.pluginVersion] {
				log.Debugf("got docker event %+v", eventMsg)
				go nuagedocker.processEvent(eventMsg)
			}
		case <-errChan:
			nuagedocker.connectionRetry <- true
			<-nuagedocker.connectionActive
			go nuagedocker.GetNetworkConnectEvents()
			return
		}
	}
}

//isSwarmEnabled checks if the docker swarm is enabled on current node
func (nuagedocker *NuageDockerClient) isSwarmEnabled() (bool, error) {
	info, err := nuagedocker.dclient.Info(context.Background())
	if err != nil {
		log.Errorf("(IsSwarmEnabled)Fetching docker node info for this node failed: %v", err)
		return false, err
	}
	if info.Swarm.LocalNodeState == swarm.LocalNodeStateActive {
		return true, nil
	}
	return false, nil
}

//isSwarmManager check if the current swarm node is manager
func (nuagedocker *NuageDockerClient) isSwarmManager() (bool, error) {
	info, err := nuagedocker.dclient.Info(context.Background())
	if err != nil {
		log.Errorf("(IsSwarmManager)Fetching docker node info for this node failed: %v", err)
		return false, err
	}
	if info.Swarm.LocalNodeState != swarm.LocalNodeStateActive {
		return false, fmt.Errorf("Swarm is not enabled on this node")
	}
	return info.Swarm.ControlAvailable, nil
}

func (nuagedocker *NuageDockerClient) buildServiceIPCache() {
	manager, err := nuagedocker.isSwarmManager()
	if manager && err == nil {
		//need another level of mutex as we are accessing map of map
		nuagedocker.Lock()
		defer nuagedocker.Unlock()
		// clear the cache
		for _, id := range nuagedocker.serviceIPCache.GetKeys() {
			nuagedocker.serviceIPCache.Write(id, nil)
		}

		services, err := nuagedocker.dclient.ServiceList(context.Background(), types.ServiceListOptions{})
		if err != nil {
			log.Errorf("Fetching list of services from docker daemon failed with error: %v", err)
			return
		}

		for _, service := range services {
			for _, vip := range service.Endpoint.VirtualIPs {
				if vip.Addr == "" {
					continue
				}
				var serviceIPMap map[string]bool
				serviceIPMapInterface, exists := nuagedocker.serviceIPCache.Read(vip.NetworkID)
				if exists {
					serviceIPMap = serviceIPMapInterface.(map[string]bool)
				} else {
					serviceIPMap = make(map[string]bool)
				}
				serviceIPMap[vip.Addr] = true
				var networkOpts *nuageConfig.NuageNetworkParams
				networkOptsIntf, inCache := nuagedocker.networkParamsTable.Read(vip.NetworkID)
				if !inCache {
					networkOpts, err = nuagedocker.GetNetworkOptsFromNetworkID(vip.NetworkID)
					if err != nil {
						log.Errorf("Fetching network opts from network ID failed with error: %v", err)
						return
					}
				} else {
					networkOpts = networkOptsIntf.(*nuageConfig.NuageNetworkParams)
				}
				nuagedocker.serviceIPCache.Write(nuageConfig.MD5Hash(networkOpts), serviceIPMap)
			}
		}
	}
	time.AfterFunc(30*time.Second, func() { nuagedocker.buildServiceIPCache() })
}

func (nuagedocker *NuageDockerClient) isServiceIP(vsdReq *nuageConfig.NuageEventMetadata) bool {
	nuagedocker.Lock()
	defer nuagedocker.Unlock()
	serviceIPMapIntf, exists := nuagedocker.serviceIPCache.Read(nuageConfig.MD5Hash(vsdReq.NetworkParams))
	if !exists {
		return false
	}
	serviceIPMap := serviceIPMapIntf.(map[string]bool)
	_, exists = serviceIPMap[vsdReq.IPAddress]
	return exists
}

func (nuagedocker *NuageDockerClient) GetOptsAllNetworks() (map[string]*nuageConfig.NuageNetworkParams, error) {
	table := make(map[string]*nuageConfig.NuageNetworkParams)
	for _, networkID := range nuagedocker.networkParamsTable.GetKeys() {
		networkParams, ok := nuagedocker.networkParamsTable.Read(networkID)
		if ok {
			table[networkID] = networkParams.(*nuageConfig.NuageNetworkParams)
		}
	}
	return table, nil
}

func (nuagedocker *NuageDockerClient) buildNetworkInfoCache() {
	networkList, err := nuagedocker.dockerNetworkList()
	if err != nil {
		log.Errorf("Fetching network list from docker failed with error %v", err)
		return
	}
	for _, network := range networkList {
		networkOpts := nuageConfig.ParseNuageParams(network.IPAM.Options)
		networkParams := &nuageConfig.NuageNetworkParams{
			Organization: networkOpts.Organization,
			Domain:       networkOpts.Domain,
			Zone:         networkOpts.Zone,
			SubnetName:   networkOpts.SubnetName,
			User:         networkOpts.User,
		}
		for _, nwConfig := range network.IPAM.Config {
			networkParams.SubnetCIDR = nwConfig.Subnet
			networkParams.Gateway = nwConfig.Gateway
		}
		nuagedocker.networkParamsTable.Write(network.ID, networkParams)
	}
	return
}

func (nuagedocker *NuageDockerClient) dockerNetworkList() ([]types.NetworkResource, error) {
	var networkList []types.NetworkResource
	var err error

	nuagedocker.executeDockerCommand(
		func() error {
			filterArgs := filters.NewArgs()
			filterArgs.Add("driver", nuageConfig.DockerNetworkType[nuagedocker.pluginVersion])
			options := types.NetworkListOptions{
				Filters: filterArgs,
			}
			networkList, err = nuagedocker.dclient.NetworkList(context.Background(), options)
			return err
		})
	if err != nil {
		log.Errorf("Retrieving existing networks from docker failed with error: %v", err)
		return networkList, err
	}
	return networkList, nil
}

//for every network connect assign the ip for the relavent endpoint id
func (nuagedocker *NuageDockerClient) processEvent(msg events.Message) {
	log.Debugf("%+v", msg)
	id := msg.Actor.Attributes["container"]
	inspect, err := nuagedocker.dclient.ContainerInspect(context.Background(), id)
	if err != nil {
		log.Errorf("Inspect on container %s failed with error %v", id, err)
	} else {
		var ip string
		networkParamsIntf, ok := nuagedocker.networkParamsTable.Read(msg.Actor.ID)
		if !ok {
			log.Errorf("NuageDockerClient: NetworkID not found in local cache")
			return
		}
		networkParams := networkParamsIntf.(*nuageConfig.NuageNetworkParams)
		for _, nwConfig := range inspect.NetworkSettings.Networks {
			if msg.Actor.ID == nwConfig.NetworkID {
				ip = nwConfig.IPAddress
			}
		}
		pg, _ := checkPolicyGroup(inspect.Config.Env)
		orchestrationID, _ := checkOrchestrationID(inspect.Config.Env)
		newReq := nuageConfig.NuageEventMetadata{
			Name:            strings.Replace(inspect.Name, "/", "", -1),
			UUID:            inspect.ID,
			PolicyGroup:     pg,
			OrchestrationID: orchestrationID,
			IPAddress:       ip,
			NetworkParams:   networkParams,
		}
		nuageApi.VSDChanRequest(nuagedocker.vsdChannel, nuageApi.VSDUpdateContainerEvent, newReq)
	}
}

func checkPolicyGroup(vars []string) (string, bool) {
	return checkEnvVar("NUAGE-POLICY-GROUP", vars)
}

func checkOrchestrationID(vars []string) (string, bool) {
	return checkEnvVar("MESOS_TASK_ID", vars)
}

func checkEnvVar(key string, envVars []string) (string, bool) {
	for _, variable := range envVars {
		if ok, err := regexp.MatchString(key, variable); ok {
			kv := strings.Split(variable, "=")
			if len(kv) == 0 {
				log.Errorf("Splitting %s in KV pair failed with error: %v", variable, err)
				return "", false
			}
			return kv[1], true
		}
	}
	return "", false
}

//Start listen for events on docker channel
func (nuagedocker *NuageDockerClient) Start() {
	log.Infof("Starting docker client")

	nuagedocker.buildNetworkInfoCache()
	nuagedocker.buildServiceIPCache()

	go nuagedocker.GetNetworkConnectEvents()

	for {
		select {
		case dockerEvent := <-nuagedocker.dockerChannel:
			go nuagedocker.handleDockerEvent(dockerEvent)
		case <-nuagedocker.connectionRetry:
			nuagedocker.handleConnectionRetry()
		case <-nuagedocker.stop:
			return
		}
	}
}

func (nuagedocker *NuageDockerClient) handleDockerEvent(event *nuageApi.DockerEvent) {
	log.Debugf("Received a docker event %+v", event)
	switch event.EventType {
	case nuageApi.DockerCheckNetworkListEvent:
		isOverlapNetwork, err := nuagedocker.CheckNetworkList(event.DockerReqObject.(*nuageConfig.NuageNetworkParams))
		event.DockerRespObjectChan <- &nuageApi.DockerRespObject{DockerData: isOverlapNetwork, Error: err}

	case nuageApi.DockerNetworkIDInspectEvent:
		networkInspect, err := nuagedocker.GetNetworkOptsFromNetworkID(event.DockerReqObject.(string))
		event.DockerRespObjectChan <- &nuageApi.DockerRespObject{DockerData: networkInspect, Error: err}

	case nuageApi.DockerPoolIDNetworkOptsEvent:
		networkInfo, err := nuagedocker.GetNetworkOptsFromPoolID(event.DockerReqObject.(string))
		event.DockerRespObjectChan <- &nuageApi.DockerRespObject{DockerData: networkInfo, Error: err}

	case nuageApi.DockerContainerListEvent:
		containerList, err := nuagedocker.GetRunningContainerList()
		event.DockerRespObjectChan <- &nuageApi.DockerRespObject{DockerData: containerList, Error: err}

	case nuageApi.DockerGetOptsAllNetworksEvent:
		networkParamsTable, err := nuagedocker.GetOptsAllNetworks()
		event.DockerRespObjectChan <- &nuageApi.DockerRespObject{DockerData: networkParamsTable, Error: err}

	case nuageApi.DockerIsSwarmEnabled:
		isSwarmEnabled, err := nuagedocker.isSwarmEnabled()
		event.DockerRespObjectChan <- &nuageApi.DockerRespObject{DockerData: isSwarmEnabled, Error: err}

	case nuageApi.DockerIsSwarmManager:
		isSwarmManager, err := nuagedocker.isSwarmManager()
		event.DockerRespObjectChan <- &nuageApi.DockerRespObject{DockerData: isSwarmManager, Error: err}

	case nuageApi.DockerIsServiceIP:
		isServiceIP := nuagedocker.isServiceIP(event.DockerReqObject.(*nuageConfig.NuageEventMetadata))
		event.DockerRespObjectChan <- &nuageApi.DockerRespObject{DockerData: isServiceIP}

	default:
		log.Errorf("NuageDockerClient: unknown api invocation")
	}
	log.Debugf("Served docker event %+v", event)
}

func (nuagedocker *NuageDockerClient) handleConnectionRetry() {
	if _, err := nuagedocker.dclient.Ping(context.Background()); err != nil {
		log.Errorf("Ping to docker host failed with error = %v. trying to reconnect", err)
		log.Errorf("will try to reconnect in every 3 seconds")
		var err error
		for {
			nuagedocker.dclient, err = connectToDockerDaemon(nuagedocker.socketFile)
			_, err = nuagedocker.dclient.Ping(context.Background())
			if err != nil {
				time.Sleep(3 * time.Second)
			} else {
				log.Infof("docker connection is now active")
				nuagedocker.connectionActive <- true
				break
			}
		}
	} else {
		nuagedocker.connectionActive <- true
	}
}

func connectToDockerDaemon(socketFile string) (*dockerClient.Client, error) {
	err := os.Setenv("DOCKER_HOST", socketFile)
	if err != nil {
		log.Errorf("Setting DOCKER_HOST failed with error: %v", err)
		return nil, err
	}
	client, err := dockerClient.NewEnvClient()
	if err != nil {
		log.Errorf("Connecting to docker client failed with error %v", err)
		return nil, err
	}
	return client, nil
}

func (nuagedocker *NuageDockerClient) executeDockerCommand(dockerCommand func() error) {
	err := dockerCommand()
	if err != nil && isDockerConnectionError(err.Error()) {
		log.Errorf(err.Error())
		nuagedocker.connectionRetry <- true
		<-nuagedocker.connectionActive
		nuagedocker.executeDockerCommand(dockerCommand)
		return
	}
	return
}

func isDockerConnectionError(errMsg string) bool {
	ok, err := regexp.MatchString("Cannot connect to the Docker daemon", errMsg)
	if err != nil {
		log.Errorf("NuageDockerClient: matching strings failed with error %v", err)
	}
	return ok
}
