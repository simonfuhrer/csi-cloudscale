/*
Copyright cloudscale.ch
Copyright 2018 DigitalOcean

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package driver

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/cloudscale-ch/cloudscale-go-sdk"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
)

const (
	// DriverName defines the name that is used in Kubernetes and the
	// system for the canonical, official name of this plugin.
	DriverName = "csi.cloudscale.ch"
)

var (
	gitTreeState = "not a git tree"
	commit       string
	version      string
)

// Driver implements the following CSI interfaces:
//
//   csi.IdentityServer
//   csi.ControllerServer
//   csi.NodeServer
//
type Driver struct {
	endpoint string
	serverId string
	region   string

	srv              *grpc.Server
	cloudscaleClient *cloudscale.Client
	mounter          Mounter
	log              *logrus.Entry

	// ready defines whether the driver is ready to function. This value will
	// be used by the `Identity` service via the `Probe()` method.
	readyMu sync.Mutex // protects ready
	ready   bool
}

// NewDriver returns a CSI plugin that contains the necessary gRPC
// interfaces to interact with Kubernetes over unix domain sockets for
// managaing cloudscale.ch Volumes
func NewDriver(ep, token, urlstr string) (*Driver, error) {
	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: token,
	})
	oauthClient := oauth2.NewClient(context.Background(), tokenSource)

	metadataClient := cloudscale.NewMetadataClient(nil)
	metadata, err := metadataClient.GetMetadata()
	if err != nil {
		return nil, fmt.Errorf("couldn't get metadata: %s", err)
	}

	// We don't have any other information than the availability zone. Just use
	// it as the region for now.
	region := metadata.AvailabilityZone
	serverId := metadata.Meta.CloudscaleUUID

	cloudscaleClient := cloudscale.NewClient(oauthClient)
	baseURL, err := url.Parse(urlstr)
	if err != nil {
		return nil, fmt.Errorf("couldn't parse url: %s", err)
	}
	cloudscaleClient.BaseURL = baseURL

	log := logrus.New().WithFields(logrus.Fields{
		"region":  region,
		"node_id": serverId,
		"version": version,
	})

	return &Driver{
		endpoint:         ep,
		serverId:         serverId,
		region:           region,
		cloudscaleClient: cloudscaleClient,
		mounter:          newMounter(log),
		log:              log,
	}, nil
}

// Run starts the CSI plugin by communication over the given endpoint
func (d *Driver) Run() error {
	u, err := url.Parse(d.endpoint)
	if err != nil {
		return fmt.Errorf("unable to parse address: %q", err)
	}

	addr := path.Join(u.Host, filepath.FromSlash(u.Path))
	if u.Host == "" {
		addr = filepath.FromSlash(u.Path)
	}

	// CSI plugins talk only over UNIX sockets currently
	if u.Scheme != "unix" {
		return fmt.Errorf("currently only unix domain sockets are supported, have: %s", u.Scheme)
	} else {
		// remove the socket if it's already there. This can happen if we
		// deploy a new version and the socket was created from the old running
		// plugin.
		d.log.WithField("socket", addr).Info("removing socket")
		if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove unix domain socket file %s, error: %s", addr, err)
		}
	}

	listener, err := net.Listen(u.Scheme, addr)
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}

	// log response errors for better observability
	errHandler := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		resp, err := handler(ctx, req)
		if err != nil {
			d.log.WithError(err).WithField("method", info.FullMethod).Error("method failed")
		}
		return resp, err
	}

	d.srv = grpc.NewServer(grpc.UnaryInterceptor(errHandler))
	csi.RegisterIdentityServer(d.srv, d)
	csi.RegisterControllerServer(d.srv, d)
	csi.RegisterNodeServer(d.srv, d)

	d.ready = true // we're now ready to go!
	d.log.WithField("addr", addr).Info("server started")
	return d.srv.Serve(listener)
}

// Stop stops the plugin
func (d *Driver) Stop() {
	d.readyMu.Lock()
	d.ready = false
	d.readyMu.Unlock()

	d.log.Info("server stopped")
	d.srv.Stop()
}

// When building any packages that import version, pass the build/install cmd
// ldflags like so:
//   go build -ldflags "-X github.com/cloudscale-ch/csi-cloudscale/driver.version=0.0.1"

// GetVersion returns the current release version, as inserted at build time.
func GetVersion() string {
	return version
}

// GetCommit returns the current commit hash value, as inserted at build time.
func GetCommit() string {
	return commit
}

// GetTreeState returns the current state of git tree, either "clean" or
// "dirty".
func GetTreeState() string {
	return gitTreeState
}
