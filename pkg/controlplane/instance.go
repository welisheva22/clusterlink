package controlplane

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"sync"

	"github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/lestrrat-go/jwx/jwk"
	"github.com/sirupsen/logrus"

	"github.com/clusterlink-net/clusterlink/pkg/api"
	event "github.com/clusterlink-net/clusterlink/pkg/controlplane/eventmanager"
	cpstore "github.com/clusterlink-net/clusterlink/pkg/controlplane/store"
	"github.com/clusterlink-net/clusterlink/pkg/platform"
	"github.com/clusterlink-net/clusterlink/pkg/policyengine"
	"github.com/clusterlink-net/clusterlink/pkg/store"
	"github.com/clusterlink-net/clusterlink/pkg/util"
)

const (
	dataplaneAppName = "cl-dataplane"
)

// Instance of a controlplane, where all API servers delegate their requested actions to.
type Instance struct {
	peerTLS *util.ParsedCertData

	peers    *cpstore.Peers
	exports  *cpstore.Exports
	imports  *cpstore.Imports
	bindings *cpstore.Bindings
	policies *cpstore.AccessPolicies

	peerLock   sync.RWMutex
	peerClient map[string]*client

	xdsManager    *xdsManager
	ports         *portManager
	policyDecider policyengine.PolicyDecider
	platform      platform.Platform

	jwkSignKey   jwk.Key
	jwkVerifyKey jwk.Key

	initialized bool

	logger *logrus.Entry
}

// CreatePeer defines a new route target for egress dataplane connections.
func (cp *Instance) CreatePeer(peer *cpstore.Peer) error {
	cp.logger.Infof("Creating peer '%s'.", peer.Name)

	if cp.initialized {
		if err := cp.peers.Create(peer); err != nil {
			return err
		}
	}

	// initialize peer client
	client := newClient(peer, cp.peerTLS.ClientConfig(peer.Name))

	cp.peerLock.Lock()
	cp.peerClient[peer.Name] = client
	cp.peerLock.Unlock()

	if err := cp.xdsManager.AddPeer(peer); err != nil {
		// practically impossible
		return err
	}

	cp.policyDecider.AddPeer(&api.Peer{Name: peer.Name, Spec: peer.PeerSpec})

	return nil
}

// UpdatePeer updates new route target for egress dataplane connections.
func (cp *Instance) UpdatePeer(peer *cpstore.Peer) error {
	cp.logger.Infof("Updating peer '%s'.", peer.Name)

	err := cp.peers.Update(peer.Name, func(old *cpstore.Peer) *cpstore.Peer {
		return peer
	})
	if err != nil {
		return err
	}

	// initialize peer client
	client := newClient(peer, cp.peerTLS.ClientConfig(peer.Name))

	cp.peerLock.Lock()
	cp.peerClient[peer.Name] = client
	cp.peerLock.Unlock()

	if err := cp.xdsManager.AddPeer(peer); err != nil {
		// practically impossible
		return err
	}

	cp.policyDecider.AddPeer(&api.Peer{Name: peer.Name, Spec: peer.PeerSpec})

	return nil
}

// GetPeer returns an existing peer.
func (cp *Instance) GetPeer(name string) *cpstore.Peer {
	cp.logger.Infof("Getting peer '%s'.", name)
	return cp.peers.Get(name)
}

// DeletePeer removes the possibility for egress dataplane connections to be routed to a given peer.
func (cp *Instance) DeletePeer(name string) (*cpstore.Peer, error) {
	cp.logger.Infof("Deleting peer '%s'.", name)

	peer, err := cp.peers.Delete(name)
	if err != nil {
		return nil, err
	}

	cp.peerClient[name].StopMonitor()
	cp.peerLock.Lock()
	delete(cp.peerClient, name)
	cp.peerLock.Unlock()

	if err := cp.xdsManager.DeletePeer(name); err != nil {
		// practically impossible
		return nil, err
	}

	cp.policyDecider.DeletePeer(name)

	return peer, nil
}

// GetAllPeers returns the list of all peers.
func (cp *Instance) GetAllPeers() []*cpstore.Peer {
	cp.logger.Info("Listing all peers.")
	return cp.peers.GetAll()
}

// CreateExport defines a new route target for ingress dataplane connections.
func (cp *Instance) CreateExport(export *cpstore.Export) error {
	cp.logger.Infof("Creating export '%s'.", export.Name)
	exSvc := export.ExportSpec.ExternalService
	if (exSvc.Host != "" && exSvc.Port == 0) || (exSvc.Host == "" && exSvc.Port != 0) {
		return fmt.Errorf("ExternalService (Host: %s ,Port: %d) wasn't set properly", exSvc.Host, exSvc.Port)
	}

	resp, err := cp.policyDecider.AddExport(&api.Export{Name: export.Name, Spec: export.ExportSpec})
	if err != nil {
		return err
	}
	if resp.Action != event.AllowAll {
		cp.logger.Warnf("Access policies deny creating export '%s'.", export.Name)
		return nil
	}

	if cp.initialized {
		if err := cp.exports.Create(export); err != nil {
			return err
		}
		// create k8s endpoint and service for external service.
		if exSvc.Host != "" && exSvc.Port != 0 && cp.initialized {
			cp.platform.CreateEndpoint(export.Name, exSvc.Host, exSvc.Port)
			cp.platform.CreateService(export.Name, export.Name, exSvc.Port, exSvc.Port)
		}
	}

	if err := cp.xdsManager.AddExport(export); err != nil {
		// practically impossible
		return err
	}

	return nil
}

// UpdateExport updates a new route target for ingress dataplane connections.
func (cp *Instance) UpdateExport(export *cpstore.Export) error {
	cp.logger.Infof("Updating export '%s'.", export.Name)
	exSvc := export.ExportSpec.ExternalService
	if (exSvc.Host != "" && exSvc.Port == 0) || (exSvc.Host == "" && exSvc.Port != 0) {
		return fmt.Errorf("ExternalService (Host: %s ,Port: %d) wasn't set properly", exSvc.Host, exSvc.Port)
	}

	resp, err := cp.policyDecider.AddExport(&api.Export{Name: export.Name, Spec: export.ExportSpec})
	if err != nil {
		return err
	}
	if resp.Action != event.AllowAll {
		cp.logger.Warnf("Access policies deny creating export '%s'.", export.Name)
		return nil
	}

	err = cp.exports.Update(export.Name, func(old *cpstore.Export) *cpstore.Export {
		return export
	})
	if err != nil {
		return err
	}
	// Update k8s endpoint and service for external service.
	if exSvc.Host != "" && exSvc.Port != 0 {
		cp.platform.UpdateEndpoint(export.Name, exSvc.Host, exSvc.Port)
		cp.platform.UpdateService(export.Name, export.Name, exSvc.Port, exSvc.Port)
	}

	if err := cp.xdsManager.AddExport(export); err != nil {
		// practically impossible
		return err
	}

	return nil
}

// GetExport returns an existing export.
func (cp *Instance) GetExport(name string) *cpstore.Export {
	cp.logger.Infof("Getting export '%s'.", name)
	return cp.exports.Get(name)
}

// DeleteExport removes the possibility for ingress dataplane connections to access a given service.
func (cp *Instance) DeleteExport(name string) (*cpstore.Export, error) {
	cp.logger.Infof("Deleting export '%s'.", name)

	export, err := cp.exports.Delete(name)
	if err != nil {
		return nil, err
	}

	// Deleting k8s endpoint and service for external service.
	exSvc := export.ExportSpec.ExternalService
	if exSvc.Host != "" && exSvc.Port != 0 {
		cp.platform.DeleteEndpoint(name)
		cp.platform.DeleteService(name)
		if err != nil {
			return nil, err
		}
	}

	if err := cp.xdsManager.DeleteExport(name); err != nil {
		// practically impossible
		return export, err
	}

	cp.policyDecider.DeleteExport(name)

	return export, nil
}

// GetAllExports returns the list of all exports.
func (cp *Instance) GetAllExports() []*cpstore.Export {
	cp.logger.Info("Listing all exports.")
	return cp.exports.GetAll()
}

// CreateImport creates a listening socket for an imported remote service.
func (cp *Instance) CreateImport(imp *cpstore.Import) error {
	cp.logger.Infof("Creating import '%s'.", imp.Name)

	port, err := cp.ports.Lease(imp.Port)
	if err != nil {
		return fmt.Errorf("cannot generate listening port: %v", err)
	}

	imp.Port = port

	if cp.initialized {
		if err := cp.imports.Create(imp); err != nil {
			cp.ports.Release(port)
			return err
		}
	}

	if err := cp.xdsManager.AddImport(imp); err != nil {
		// practically impossible
		return err
	}

	// TODO: handle a crash happening between storing an import and creating a service
	if cp.initialized {
		cp.platform.CreateService(imp.Service.Host, dataplaneAppName, imp.Service.Port, imp.Port)
	}

	return nil
}

// UpdateImport updates a listening socket for an imported remote service.
func (cp *Instance) UpdateImport(imp *cpstore.Import) error {
	cp.logger.Infof("Updating import '%s'.", imp.Name)

	err := cp.imports.Update(imp.Name, func(old *cpstore.Import) *cpstore.Import {
		imp.Port = old.Port
		return imp
	})
	if err != nil {
		return err
	}

	if err := cp.xdsManager.AddImport(imp); err != nil {
		// practically impossible
		return err
	}

	cp.platform.UpdateService(imp.Service.Host, dataplaneAppName, imp.Service.Port, imp.Port)

	return nil
}

// GetImport returns an existing import.
func (cp *Instance) GetImport(name string) *cpstore.Import {
	cp.logger.Infof("Getting import '%s'.", name)
	return cp.imports.Get(name)
}

// DeleteImport removes the listening socket of a previously imported service.
func (cp *Instance) DeleteImport(name string) (*cpstore.Import, error) {
	cp.logger.Infof("Deleting import '%s'.", name)

	imp, err := cp.imports.Delete(name)
	if err != nil {
		return nil, err
	}

	if err := cp.xdsManager.DeleteImport(name); err != nil {
		// practically impossible
		return imp, err
	}

	cp.ports.Release(imp.Port)

	cp.platform.DeleteService(imp.Service.Host)

	return imp, nil
}

// GetAllImports returns the list of all imports.
func (cp *Instance) GetAllImports() []*cpstore.Import {
	cp.logger.Info("Listing all imports.")
	return cp.imports.GetAll()
}

// CreateBinding creates a binding of an imported service to a remote exported service.
func (cp *Instance) CreateBinding(binding *cpstore.Binding) error {
	cp.logger.Infof("Creating binding '%s'->'%s'.", binding.Import, binding.Peer)

	action, err := cp.policyDecider.AddBinding(&api.Binding{Spec: binding.BindingSpec})
	if err != nil {
		return err
	}
	if action != event.Allow {
		cp.logger.Warnf("Access policies deny creating binding '%s'->'%s' .", binding.Import, binding.Peer)
		return nil
	}

	if cp.initialized {
		if err := cp.bindings.Create(binding); err != nil {
			return err
		}
	}

	return nil
}

// UpdateBinding updates a binding of an imported service to a remote exported service.
func (cp *Instance) UpdateBinding(binding *cpstore.Binding) error {
	cp.logger.Infof("Updating binding '%s'->'%s'.", binding.Import, binding.Peer)

	action, err := cp.policyDecider.AddBinding(&api.Binding{Spec: binding.BindingSpec})
	if err != nil {
		return err
	}
	if action != event.Allow {
		cp.logger.Warnf("Access policies deny creating binding '%s'->'%s' .", binding.Import, binding.Peer)
		return nil
	}

	err = cp.bindings.Update(binding, func(old *cpstore.Binding) *cpstore.Binding {
		return binding
	})
	if err != nil {
		return err
	}

	return nil
}

// GetBindings returns all bindings for a given imported service.
func (cp *Instance) GetBindings(imp string) []*cpstore.Binding {
	cp.logger.Infof("Getting bindings for import '%s'.", imp)
	return cp.bindings.Get(imp)
}

// DeleteBinding removes a binding of an imported service to a remote exported service.
func (cp *Instance) DeleteBinding(binding *cpstore.Binding) (*cpstore.Binding, error) {
	cp.logger.Infof("Deleting binding '%s'->'%s'.", binding.Import, binding.Peer)

	cp.policyDecider.DeleteBinding(&api.Binding{Spec: binding.BindingSpec})

	return cp.bindings.Delete(binding)
}

// GetAllBindings returns the list of all bindings.
func (cp *Instance) GetAllBindings() []*cpstore.Binding {
	cp.logger.Info("Listing all bindings.")
	return cp.bindings.GetAll()
}

// CreateAccessPolicy creates an access policy to allow/deny specific connections.
func (cp *Instance) CreateAccessPolicy(policy *cpstore.AccessPolicy) error {
	cp.logger.Infof("Creating access policy '%s'.", policy.Spec.Blob)

	if cp.initialized {
		if err := cp.policies.Create(policy); err != nil {
			return err
		}
	}

	return cp.policyDecider.AddAccessPolicy(&api.Policy{Spec: policy.Spec})
}

// UpdateAccessPolicy updates an access policy to allow/deny specific connections.
func (cp *Instance) UpdateAccessPolicy(policy *cpstore.AccessPolicy) error {
	cp.logger.Infof("Updating access policy '%s'.", policy.Spec.Blob)

	err := cp.policies.Update(policy.Name, func(old *cpstore.AccessPolicy) *cpstore.AccessPolicy {
		return policy
	})
	if err != nil {
		return err
	}

	return cp.policyDecider.AddAccessPolicy(&api.Policy{Spec: policy.Spec})
}

// DeleteAccessPolicy removes an access policy to allow/deny specific connections.
func (cp *Instance) DeleteAccessPolicy(policy *cpstore.AccessPolicy) (*cpstore.AccessPolicy, error) {
	cp.logger.Infof("Deleting access policy '%s'.", policy.Spec.Blob)

	if err := cp.policyDecider.DeleteAccessPolicy(&api.Policy{Spec: policy.Spec}); err != nil {
		return nil, err
	}

	return cp.policies.Delete(policy.Name)
}

// GetAccessPolicy returns an access policy with the given name.
func (cp *Instance) GetAccessPolicy(name string) *cpstore.AccessPolicy {
	cp.logger.Infof("Getting access policy '%s'.", name)
	return cp.policies.Get(name)
}

// GetAllAccessPolicies returns the list of all AccessPolicies.
func (cp *Instance) GetAllAccessPolicies() []*cpstore.AccessPolicy {
	cp.logger.Info("Listing all access policies.")
	return cp.policies.GetAll()
}

// GetXDSClusterManager returns the xDS cluster manager.
func (cp *Instance) GetXDSClusterManager() cache.Cache {
	return cp.xdsManager.clusters
}

// GetXDSListenerManager returns the xDS listener manager.
func (cp *Instance) GetXDSListenerManager() cache.Cache {
	return cp.xdsManager.listeners
}

// init initializes the controlplane manager.
func (cp *Instance) init() error {
	// generate the JWK key
	if err := cp.generateJWK(); err != nil {
		return fmt.Errorf("unable to generate JWK key: %v", err)
	}

	// add peers
	for _, p := range cp.GetAllPeers() {
		if err := cp.CreatePeer(p); err != nil {
			return err
		}
	}

	// add exports
	for _, export := range cp.GetAllExports() {
		if err := cp.CreateExport(export); err != nil {
			return err
		}
	}

	// add exports
	for _, imp := range cp.GetAllImports() {
		if err := cp.CreateImport(imp); err != nil {
			return err
		}
	}

	// add bindings
	for _, binding := range cp.GetAllBindings() {
		if err := cp.CreateBinding(binding); err != nil {
			return err
		}
	}

	// add policies
	for _, policy := range cp.GetAllAccessPolicies() {
		if err := cp.CreateAccessPolicy(policy); err != nil {
			return err
		}
	}

	cp.initialized = true

	return nil
}

// generateJWK generates a new JWK for signing JWT access tokens.
func (cp *Instance) generateJWK() error {
	cp.logger.Infof("Updating the JWK.")

	// generate RSA key-pair
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return fmt.Errorf("unable to generate RSA keys: %v", err)
	}

	jwkSignKey, err := jwk.New(rsaKey)
	if err != nil {
		return fmt.Errorf("unable to create JWK signing key: %v", err)
	}

	jwkVerifyKey, err := jwk.New(rsaKey.PublicKey)
	if err != nil {
		return fmt.Errorf("unable to create JWK verifing key: %v", err)
	}

	cp.jwkSignKey = jwkSignKey
	cp.jwkVerifyKey = jwkVerifyKey
	return nil
}

// NewInstance returns a new controlplane instance.
func NewInstance(peerTLS *util.ParsedCertData, storeManager store.Manager, platform platform.Platform) (*Instance, error) {
	logger := logrus.WithField("component", "controlplane")

	peers, err := cpstore.NewPeers(storeManager)
	if err != nil {
		return nil, fmt.Errorf("cannot load peers from store: %v", err)
	}
	logger.Infof("Loaded %d peers.", peers.Len())

	exports, err := cpstore.NewExports(storeManager)
	if err != nil {
		return nil, fmt.Errorf("cannot load exports from store: %v", err)
	}
	logger.Infof("Loaded %d exports.", exports.Len())

	imports, err := cpstore.NewImports(storeManager)
	if err != nil {
		return nil, fmt.Errorf("cannot load imports from store: %v", err)
	}
	logger.Infof("Loaded %d imports.", imports.Len())

	bindings, err := cpstore.NewBindings(storeManager)
	if err != nil {
		return nil, fmt.Errorf("cannot load bindings from store: %v", err)
	}
	logger.Infof("Loaded %d bindings.", bindings.Len())

	policies, err := cpstore.NewAccessPolicies(storeManager)
	if err != nil {
		return nil, fmt.Errorf("cannot load access policies from store: %v", err)
	}
	logger.Infof("Loaded %d access policies.", policies.Len())

	cp := &Instance{
		peerTLS:       peerTLS,
		peerClient:    make(map[string]*client),
		peers:         peers,
		exports:       exports,
		imports:       imports,
		bindings:      bindings,
		policies:      policies,
		xdsManager:    newXDSManager(),
		ports:         newPortManager(),
		policyDecider: policyengine.NewPolicyHandler(),
		platform:      platform,
		initialized:   false,
		logger:        logger,
	}

	// initialize instance
	if err := cp.init(); err != nil {
		return nil, err
	}

	return cp, nil
}
