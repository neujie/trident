// Copyright 2019 NetApp, Inc. All Rights Reserved.

package storageclass

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"regexp"
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/netapp/trident/config"
	. "github.com/netapp/trident/logger"
	"github.com/netapp/trident/storage"
	storageattribute "github.com/netapp/trident/storage_attribute"
)

type BackendPoolInfo struct {
	Pools             []*storage.Pool
	PhysicalPoolNames map[string]struct{}
}

func New(c *Config) *StorageClass {
	if c.Version == "" {
		c.Version = config.OrchestratorAPIVersion
	}
	return &StorageClass{
		config: c,
		pools:  make([]*storage.Pool, 0),
	}
}

func NewForConfig(configJSON string) (*StorageClass, error) {
	var scConfig Config
	err := json.Unmarshal([]byte(configJSON), &scConfig)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal config: %v", err)
	}
	return New(&scConfig), nil
}

func NewFromPersistent(persistent *Persistent) *StorageClass {
	return New(persistent.Config)
}

func NewFromAttributes(attributes map[string]storageattribute.Request) *StorageClass {

	cfg := &Config{
		Version:         "1",
		Attributes:      attributes,
		Pools:           make(map[string][]string),
		AdditionalPools: make(map[string][]string),
		ExcludePools:    make(map[string][]string),
	}
	return &StorageClass{
		config: cfg,
		pools:  make([]*storage.Pool, 0),
	}
}

func (s *StorageClass) regexMatcherImpl(
	ctx context.Context, storagePool *storage.Pool, storagePoolBackendName string, storagePoolList []string,
) bool {

	if storagePool == nil {
		return false
	}
	if storagePoolBackendName == "" {
		return false
	}
	if storagePoolList == nil {
		return false
	}

	if !strings.HasPrefix(storagePoolBackendName, "^") {
		storagePoolBackendName = "^" + storagePoolBackendName
	}
	if !strings.HasSuffix(storagePoolBackendName, "$") {
		storagePoolBackendName = storagePoolBackendName + "$"
	}

	poolsMatch := false
	for _, storagePoolName := range storagePoolList {
		backendMatch, err := regexp.MatchString(storagePoolBackendName, storagePool.Backend.Name)
		if err != nil {
			Logc(ctx).WithFields(log.Fields{
				"storagePoolName":          storagePoolName,
				"storagePool.Name":         storagePool.Name,
				"storagePool.Backend.Name": storagePool.Backend.Name,
				"storagePoolBackendName":   storagePoolBackendName,
				"err":                      err,
			}).Warning("Error comparing backend names in regexMatcher.")
			continue
		}
		Logc(ctx).WithFields(log.Fields{
			"storagePool.Backend.Name": storagePool.Backend.Name,
			"storagePoolBackendName":   storagePoolBackendName,
			"backendMatch":             backendMatch,
		}).Debug("Compared backend names in regexMatcher.")
		if !backendMatch {
			continue
		}

		matched, err := regexp.MatchString(storagePoolName, storagePool.Name)
		if err != nil {
			Logc(ctx).WithFields(log.Fields{
				"storagePoolName":          storagePoolName,
				"storagePool.Name":         storagePool.Name,
				"storagePool.Backend.Name": storagePool.Backend.Name,
				"poolsMatch":               poolsMatch,
				"err":                      err,
			}).Warning("Error comparing pool names in regexMatcher.")
			continue
		}
		if matched {
			poolsMatch = true
		}
		Logc(ctx).WithFields(log.Fields{
			"storagePoolName":          storagePoolName,
			"storagePool.Name":         storagePool.Name,
			"storagePool.Backend.Name": storagePool.Backend.Name,
			"poolsMatch":               poolsMatch,
		}).Debug("Compared pool names in regexMatcher.")
	}
	return poolsMatch
}

func (s *StorageClass) regexMatcher(ctx context.Context, storagePool *storage.Pool, poolMap map[string][]string) bool {
	poolsMatch := false
	if len(poolMap) > 0 {
		for storagePoolBackendName, storagePoolList := range poolMap {
			poolsMatch = s.regexMatcherImpl(ctx, storagePool, storagePoolBackendName, storagePoolList)
			if poolsMatch {
				return true
			}
		}
	}
	return poolsMatch
}

func (s *StorageClass) Matches(ctx context.Context, storagePool *storage.Pool) bool {

	Logc(ctx).WithFields(log.Fields{
		"storageClass": s.GetName(),
		"config":       s.config,
		"pool":         storagePool.Name,
		"poolBackend":  storagePool.Backend.Name,
	}).Debug("Checking if storage pool matches.")

	// Check excludeStoragePools first, since it can reject a match
	if len(s.config.ExcludePools) > 0 {
		if matches := s.regexMatcher(ctx, storagePool, s.config.ExcludePools); matches {
			return false
		}
	}

	// Check additionalStoragePools next, since it can yield a match result by itself
	if len(s.config.AdditionalPools) > 0 {
		if matches := s.regexMatcher(ctx, storagePool, s.config.AdditionalPools); matches {
			return true
		}

		// Handle the sub-case where additionalStoragePools is specified (but didn't match) and
		// there are no attributes or storagePools specified in the storage class.  This should
		// always return false.
		if len(s.config.Attributes) == 0 && len(s.config.Pools) == 0 {
			Logc(ctx).WithFields(log.Fields{
				"storageClass": s.GetName(),
				"pool":         storagePool.Name,
			}).Debug("Pool failed to match storage class additionalStoragePools attribute.")
			return false
		}
	}

	// Attributes are used to narrow the pool selection.  Therefore if no attributes are
	// specified, then all pools can match.  If one or more attributes are specified in the
	// storage class, then all must match.
	attributesMatch := true
	for name, request := range s.config.Attributes {

		// Remap the "selector" storage class attribute to the "labels" pool attribute
		if name == "selector" {
			name = "labels"
		}

		if offer, ok := storagePool.Attributes[name]; !ok || !offer.Matches(request) {
			Logc(ctx).WithFields(log.Fields{
				"offer":        offer,
				"request":      request,
				"storageClass": s.GetName(),
				"pool":         storagePool.Name,
				"attribute":    name,
				"found":        ok,
			}).Debug("Attribute for storage pool failed to match storage class.")
			attributesMatch = false
			break
		}
	}

	// The storagePools list is used to narrow the pool selection.  Therefore, if no pools are
	// specified, then all pools can match.  If one or more pools are listed in the storage
	// class, then the pool must be in the list.
	poolsMatch := true
	if len(s.config.Pools) > 0 {
		poolsMatch = s.regexMatcher(ctx, storagePool, s.config.Pools)
	}

	result := attributesMatch && poolsMatch

	Logc(ctx).WithFields(log.Fields{
		"attributesMatch": attributesMatch,
		"poolsMatch":      poolsMatch,
		"match":           result,
		"pool":            storagePool.Name,
		"storageClass":    s.GetName(),
	}).Debug("Result of pool match for storage class.")

	return result
}

// CheckAndAddBackend iterates through each of the storage pools
// for a given backend.  If the pool satisfies the storage class, it
// adds that pool.  Returns the number of storage pools added.
func (s *StorageClass) CheckAndAddBackend(ctx context.Context, b *storage.Backend) int {

	Logc(ctx).WithFields(log.Fields{
		"backend":      b.Name,
		"storageClass": s.GetName(),
	}).Debug("Checking backend for storage class")

	if !b.State.IsOnline() {
		Logc(ctx).WithField("backend", b.Name).Warn("Backend not online.")
		return 0
	}

	added := 0
	for _, storagePool := range b.Storage {
		if s.Matches(ctx, storagePool) {
			s.pools = append(s.pools, storagePool)
			storagePool.AddStorageClass(s.GetName())
			added++
			Logc(ctx).WithFields(log.Fields{
				"pool":         storagePool.Name,
				"storageClass": s.GetName(),
			}).Debug("Storage class added to the storage pool.")
		}
	}
	return added
}

func (s *StorageClass) IsAddedToBackend(backend *storage.Backend, storageClassName string) bool {

	for _, storagePool := range backend.Storage {
		for _, storageClass := range storagePool.StorageClasses {
			if storageClass == storageClassName {
				return true
			}
		}
	}

	return false
}

func (s *StorageClass) RemovePoolsForBackend(backend *storage.Backend) {
	newStoragePools := make([]*storage.Pool, 0)
	for _, storagePool := range s.pools {
		if storagePool.Backend != backend {
			newStoragePools = append(newStoragePools, storagePool)
		}
	}
	s.pools = newStoragePools
}

func (s *StorageClass) GetAttributes() map[string]storageattribute.Request {
	return s.config.Attributes
}

func (s *StorageClass) GetName() string {
	return s.config.Name
}

func (s *StorageClass) GetStoragePools() map[string][]string {
	return s.config.Pools
}

func (s *StorageClass) GetAdditionalStoragePools() map[string][]string {
	return s.config.AdditionalPools
}

func (s *StorageClass) GetStoragePoolsForProtocol(ctx context.Context, p config.Protocol) []*storage.Pool {
	ret := make([]*storage.Pool, 0, len(s.pools))
	// TODO:  Change this to work with indices of backends?
	for _, storagePool := range s.pools {
		if p == config.ProtocolAny || storagePool.Backend.GetProtocol(ctx) == p {
			ret = append(ret, storagePool)
		}
	}
	return ret
}

// GetStoragePoolsForProtocolByBackend returns a map of backend to list of pools on that backend, where
// each pool matches the supplied protocol.  Each pool list is shuffled, so the caller may use the list
// to select backends and pools at random.  The caller may assume that each value in the map is a list
// containing at least one pool.
func (s *StorageClass) GetStoragePoolsForProtocolByBackend(
	ctx context.Context, p config.Protocol, requisiteTopologies, preferredTopologies []map[string]string,
) map[string]*BackendPoolInfo {

	// Get all matching pools
	pools := s.GetStoragePoolsForProtocol(ctx, p)

	// Build a map of backends to a list of matching pools and physical pool names on each backend
	poolMap := make(map[string]*BackendPoolInfo)
	for _, pool := range pools {
		if _, ok := poolMap[pool.Backend.Name]; !ok {
			// Get Names of physical Pools associated with this backend
			physicalPoolNames := pool.Backend.GetPhysicalPoolNames(ctx)
			physicalPoolNamesMap := make(map[string]struct{})
			for _, physicalPoolName := range physicalPoolNames {
				physicalPoolNamesMap[physicalPoolName] = struct{}{}
			}

			poolMap[pool.Backend.Name] = &BackendPoolInfo{Pools: make([]*storage.Pool, 0),
				PhysicalPoolNames: physicalPoolNamesMap}
		}
		poolMap[pool.Backend.Name].Pools = append(poolMap[pool.Backend.Name].Pools, pool)
	}

	// Shuffle the pools in each backend list
	for _, backendPoolInfo := range poolMap {
		backendPools := backendPoolInfo.Pools
		rand.Shuffle(len(backendPools), func(i, j int) {
			backendPools[i], backendPools[j] = backendPools[j], backendPools[i]
		})
	}

	return poolMap
}

func (s *StorageClass) Pools() []*storage.Pool {
	return s.pools
}

func (s *StorageClass) ConstructExternal(ctx context.Context) *External {

	ret := &External{
		Config:       s.config,
		StoragePools: make(map[string][]string),
	}
	for _, storagePool := range s.pools {
		backendName := storagePool.Backend.Name
		if storagePoolList, ok := ret.StoragePools[backendName]; ok {
			Logc(ctx).WithFields(log.Fields{
				"storageClass": s.GetName(),
				"pool":         storagePool.Name,
				"Backend":      backendName,
				"Method":       "ConstructExternal",
			}).Debug("Appending to existing storage pool list for backend.")
			ret.StoragePools[backendName] = append(storagePoolList, storagePool.Name)
		} else {
			Logc(ctx).WithFields(log.Fields{
				"storageClass": s.GetName(),
				"pool":         storagePool.Name,
				"Backend":      backendName,
				"Method":       "ConstructExternal",
			}).Debug("Creating new storage pool list for backend.")
			ret.StoragePools[backendName] = make([]string, 1, 1)
			ret.StoragePools[backendName][0] = storagePool.Name
		}
	}
	for _, list := range ret.StoragePools {
		sort.Strings(list)
	}
	for _, list := range ret.Config.Pools {
		sort.Strings(list)
	}
	for _, list := range ret.Config.AdditionalPools {
		sort.Strings(list)
	}
	return ret
}

func (s *External) GetName() string {
	return s.Config.Name
}

func (s *StorageClass) ConstructPersistent() *Persistent {
	ret := &Persistent{Config: s.config}
	for _, list := range ret.Config.Pools {
		sort.Strings(list)
	}
	for _, list := range ret.Config.AdditionalPools {
		sort.Strings(list)
	}
	return ret
}

func (s *Persistent) GetName() string {
	return s.Config.Name
}
