package main

import (
	"context"
	"net"
	"os"
	"path"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/mpreu/k8s-device-plugin-v4l2loopback/v4l2l"
	"google.golang.org/grpc"
	api "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta1"
)

const (
	// pluginSocket describes the local path to the socket file on the system.
	pluginSocket = api.DevicePluginPath + "v4l2l.sock"
	resourceName = "mpreu.de/v4l2l"
)

// V4l2lDevicePlugin is the type which implements the Kubernetes
// device plugin interface.
type V4l2lDevicePlugin struct {
	resourceName string
	socketName   string
	deviceMap    map[string]v4l2l.Device
	devices      []*api.Device
	server       *grpc.Server
}

// NewV4l2lDevicePlugin constructs a V4l2lDevicePlugin
func NewV4l2lDevicePlugin() *V4l2lDevicePlugin {

	devMap := make(map[string]v4l2l.Device)
	var devices []*api.Device

	for _, device := range v4l2l.GetDeviceList() {
		id := device.Name
		devMap[id] = device
		devices = append(devices, &api.Device{
			ID:     id,
			Health: api.Healthy,
		})
	}

	return &V4l2lDevicePlugin{
		resourceName: resourceName,
		socketName:   pluginSocket,
		deviceMap:    devMap,
		devices:      devices,
	}
}

// GetDevicePluginOptions return options for the device plugin.
// Implementation of the 'DevicePluginServer' interface.
func (plugin *V4l2lDevicePlugin) GetDevicePluginOptions(context.Context, *api.Empty) (*api.DevicePluginOptions, error) {
	return &api.DevicePluginOptions{
		PreStartRequired: false,
	}, nil
}

// Register registers the device plugin with the given resource name with the Kubelet.
func (plugin *V4l2lDevicePlugin) Register(kubeletEndpoint string, resourceName string) error {

	log.Debugln("Entering register function")

	conn, err := checkServerConnection(kubeletEndpoint)
	if err != nil {
		log.Errorf("Cannot establish connection to Kubelet endpoint: %v", err)
		return err
	}
	defer conn.Close()

	client := api.NewRegistrationClient(conn)

	request := &api.RegisterRequest{
		Version:      api.Version,
		Endpoint:     path.Base(pluginSocket),
		ResourceName: plugin.resourceName,
	}

	log.Debugf("RegisterRequest: %v", request)

	_, err = client.Register(context.Background(), request)
	if err != nil {
		log.Errorf("Sending plugin register request failed: %v", err)
		return err
	}

	return nil

}

// ListAndWatch communicates changes of device states and returns a
// new device list. Implementation of the 'DevicePluginServer' interface.
func (plugin *V4l2lDevicePlugin) ListAndWatch(e *api.Empty, s api.DevicePlugin_ListAndWatchServer) error {
	log.Debugf("ListAndWatch devices: %v", plugin.devices)
	response := api.ListAndWatchResponse{
		Devices: plugin.devices,
	}
	err := s.Send(&response)

	if err != nil {
		log.Errorf("Error when sending ListAndWatch response: %v", err)
		return err
	}

	for {
		select {}
	}
}

// Allocate is resposible to make the device available during the
// container creation process. Implementation of the 'DevicePluginServer' interface.
func (plugin *V4l2lDevicePlugin) Allocate(ctx context.Context, request *api.AllocateRequest) (*api.AllocateResponse, error) {

	log.Debugf("Allocate request: %v", request.GetContainerRequests())

	responses := make([]*api.ContainerAllocateResponse, len(request.GetContainerRequests()))

	for i, ctnRequest := range request.GetContainerRequests() {
		specs := createDeviceSpecs(plugin, ctnRequest)

		r := &api.ContainerAllocateResponse{
			Devices: specs,
		}
		responses[i] = r
	}

	response := api.AllocateResponse{
		ContainerResponses: responses,
	}

	log.Debugf("Allocate response: %v", response)

	return &response, nil
}

// PreStartContainer is called during registration phase of a container.
// Implementation of the 'DevicePluginServer' interface.
func (plugin *V4l2lDevicePlugin) PreStartContainer(context.Context, *api.PreStartContainerRequest) (*api.PreStartContainerResponse, error) {
	return &api.PreStartContainerResponse{}, nil
}

// StartServer starts the gRPC server of the device plugin
func (plugin *V4l2lDevicePlugin) StartServer() error {
	plugin.server = grpc.NewServer([]grpc.ServerOption{}...)

	listener, err := net.Listen("unix", pluginSocket)

	if err != nil {
		return err
	}

	api.RegisterDevicePluginServer(plugin.server, plugin)

	go plugin.server.Serve(listener)

	// Be sure the connection is established
	conn, err := checkServerConnection(pluginSocket)
	if err != nil {
		return err
	}
	conn.Close()

	return nil
}

// StopServer stops the gRPC server of the device plugin.
func (plugin *V4l2lDevicePlugin) StopServer() error {
	if plugin.server == nil {
		return nil
	}

	plugin.server.Stop()
	plugin.server = nil

	return cleanupSocket()
}

// Serve starts the gRPC server and registers the device plugin to the Kubelet.
func (plugin *V4l2lDevicePlugin) Serve() error {
	err := plugin.StartServer()
	if err != nil {
		log.Errorf("Could not start device plugin gRPC server: %v", err)
		return err
	}

	log.Debugln("Start registering plugin to Kubelet")

	err = plugin.Register(api.KubeletSocket, plugin.resourceName)
	if err != nil {
		log.Errorf("Could not register device plugin to Kubelet: %s", err)
		plugin.StopServer()
		return err
	}

	log.Debugln("Registered device plugin to Kubelet")

	return nil
}

// CleanupSocket deletes the socket for the device plugin
func cleanupSocket() error {
	if err := os.Remove(pluginSocket); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

// createDeviceSpec returns a kubernetes device spec for the
// device plugin api based on a V4l2l device.
func createDeviceSpec(d *v4l2l.Device) *api.DeviceSpec {
	return &api.DeviceSpec{
		ContainerPath: d.Path,
		HostPath:      d.Path,
		Permissions:   "rw",
	}
}

// createDeviceSpecs returns a list of kubernetes device specs
// for the device plugin api. Based on a allocate request of a
// kubelet the corresponding V4l2l devices are selected.
func createDeviceSpecs(plugin *V4l2lDevicePlugin, request *api.ContainerAllocateRequest) []*api.DeviceSpec {
	deviceIDs := request.GetDevicesIDs()
	var specs []*api.DeviceSpec

	for _, deviceID := range deviceIDs {
		log.Debugf("Process 'Allocate' for deviceID: %s", deviceID)

		currentDevice := plugin.deviceMap[deviceID]
		ds := createDeviceSpec(&currentDevice)
		specs = append(specs, ds)

	}
	return specs
}

// checkServerConnection tests the gRPC server of the device plugin.
// If no connection to the corresponding unix socket can be established
// it is considered as an error.
func checkServerConnection(endpoint string) (*grpc.ClientConn, error) {
	timeout := 5 * time.Second

	c, err := grpc.Dial(endpoint,
		grpc.WithInsecure(),
		grpc.WithTimeout(timeout),
		grpc.WithBlock(),
		grpc.WithDialer(func(target string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", target, timeout)
		}))

	if err != nil {
		return nil, err
	}

	return c, nil
}
