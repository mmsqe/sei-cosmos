package rootmulti

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"

	"cosmossdk.io/errors"
	snapshottypes "github.com/cosmos/cosmos-sdk/snapshots/types"
	"github.com/cosmos/cosmos-sdk/store/cachemulti"
	"github.com/cosmos/cosmos-sdk/store/mem"
	"github.com/cosmos/cosmos-sdk/store/rootmulti"
	"github.com/cosmos/cosmos-sdk/store/transient"
	"github.com/cosmos/cosmos-sdk/store/types"
	"github.com/cosmos/cosmos-sdk/storev2/commitment"
	"github.com/cosmos/cosmos-sdk/storev2/state"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	protoio "github.com/gogo/protobuf/io"
	commonerrors "github.com/sei-protocol/sei-db/common/errors"
	"github.com/sei-protocol/sei-db/config"
	"github.com/sei-protocol/sei-db/proto"
	"github.com/sei-protocol/sei-db/sc"
	sctypes "github.com/sei-protocol/sei-db/sc/types"
	"github.com/sei-protocol/sei-db/ss"
	"github.com/sei-protocol/sei-db/ss/pruning"
	sstypes "github.com/sei-protocol/sei-db/ss/types"
	abci "github.com/tendermint/tendermint/abci/types"
	"github.com/tendermint/tendermint/libs/log"
	dbm "github.com/tendermint/tm-db"
)

var (
	_ types.CommitMultiStore = (*Store)(nil)
	_ types.Queryable        = (*Store)(nil)
)

type Store struct {
	logger         log.Logger
	mtx            sync.RWMutex
	scStore        sctypes.Committer
	ssStore        sstypes.StateStore
	lastCommitInfo *types.CommitInfo
	storesParams   map[types.StoreKey]storeParams
	storeKeys      map[string]types.StoreKey
	ckvStores      map[types.StoreKey]types.CommitKVStore
	pendingChanges chan VersionedChangesets
	pruningManager *pruning.Manager
}

type VersionedChangesets struct {
	Version    int64
	Changesets []*proto.NamedChangeSet
}

func NewStore(
	homeDir string,
	logger log.Logger,
	scConfig config.StateCommitConfig,
	ssConfig config.StateStoreConfig,
) *Store {
	scStore := sc.NewCommitStore(homeDir, logger, scConfig)
	store := &Store{
		logger:         logger,
		scStore:        scStore,
		storesParams:   make(map[types.StoreKey]storeParams),
		storeKeys:      make(map[string]types.StoreKey),
		ckvStores:      make(map[types.StoreKey]types.CommitKVStore),
		pendingChanges: make(chan VersionedChangesets, 1000),
	}
	if ssConfig.Enable {
		ssStore, err := ss.NewStateStore(homeDir, ssConfig)
		if err != nil {
			panic(err)
		}
		if err = ss.RecoverStateStore(homeDir, logger, ssStore); err != nil {
			panic(err)
		}
		store.ssStore = ssStore
		go store.StateStoreCommit()
		store.pruningManager = pruning.NewPruningManager(
			logger, ssStore, int64(ssConfig.KeepRecent), int64(ssConfig.PruneIntervalSeconds))
		store.pruningManager.Start()
	}
	return store

}

// Commit implements interface Committer, called by ABCI Commit
func (rs *Store) Commit(bumpVersion bool) types.CommitID {
	if !bumpVersion {
		return rs.lastCommitInfo.CommitID()
	}
	if err := rs.flush(); err != nil {
		panic(err)
	}

	rs.mtx.Lock()
	defer rs.mtx.Unlock()
	for _, store := range rs.ckvStores {
		if store.GetStoreType() != types.StoreTypeIAVL {
			_ = store.Commit(bumpVersion)
		}
	}
	// Commit to SC Store
	_, err := rs.scStore.Commit()
	if err != nil {
		panic(err)
	}

	// The underlying sc store might be reloaded, reload the store as well.
	for key := range rs.ckvStores {
		store := rs.ckvStores[key]
		if store.GetStoreType() == types.StoreTypeIAVL {
			rs.ckvStores[key], err = rs.loadCommitStoreFromParams(key, rs.storesParams[key])
			if err != nil {
				panic(fmt.Errorf("inconsistent store map, store %s not found", key.Name()))
			}
		}
	}

	rs.lastCommitInfo = convertCommitInfo(rs.scStore.LastCommitInfo())
	rs.lastCommitInfo = amendCommitInfo(rs.lastCommitInfo, rs.storesParams)
	return rs.lastCommitInfo.CommitID()
}

// StateStoreCommit is a background routine to apply changes to SS store
func (rs *Store) StateStoreCommit() {
	for pendingChangeSet := range rs.pendingChanges {
		version := pendingChangeSet.Version
		for _, cs := range pendingChangeSet.Changesets {
			if err := rs.ssStore.ApplyChangeset(version, cs); err != nil {
				panic(err)
			}
		}
	}
}

// Flush all the pending changesets to commit store.
func (rs *Store) flush() error {
	var changeSets []*proto.NamedChangeSet
	currentVersion := rs.lastCommitInfo.Version
	for key := range rs.ckvStores {
		// it'll unwrap the inter-block cache
		store := rs.GetCommitKVStore(key)
		if commitStore, ok := store.(*commitment.Store); ok {
			cs := commitStore.PopChangeSet()
			if len(cs.Pairs) > 0 {
				changeSets = append(changeSets, &proto.NamedChangeSet{
					Name:      key.Name(),
					Changeset: cs,
				})
			}
		}
	}
	if changeSets != nil && len(changeSets) > 0 {
		sort.SliceStable(changeSets, func(i, j int) bool {
			return changeSets[i].Name < changeSets[j].Name
		})
		if rs.ssStore != nil {
			rs.pendingChanges <- VersionedChangesets{
				Version:    currentVersion,
				Changesets: changeSets,
			}
		}
	}
	return rs.scStore.ApplyChangeSets(changeSets)
}

func (rs *Store) Close() error {
	err := rs.scStore.Close()
	close(rs.pendingChanges)
	if rs.ssStore != nil {
		err = commonerrors.Join(err, rs.ssStore.Close())
	}
	return err
}

// LastCommitID Implements interface Committer
func (rs *Store) LastCommitID() types.CommitID {
	if rs.lastCommitInfo == nil {
		v, err := rs.scStore.GetLatestVersion()
		if err != nil {
			panic(fmt.Errorf("failed to get latest version: %w", err))
		}
		return types.CommitID{Version: v}
	}

	return rs.lastCommitInfo.CommitID()
}

// Implements interface Committer
func (rs *Store) SetPruning(types.PruningOptions) {
}

// Implements interface Committer
func (rs *Store) GetPruning() types.PruningOptions {
	return types.PruneDefault
}

// Implements interface Store
func (rs *Store) GetStoreType() types.StoreType {
	return types.StoreTypeMulti
}

// Implements interface CacheWrapper
func (rs *Store) CacheWrap(storeKey types.StoreKey) types.CacheWrap {
	return rs.CacheMultiStore().CacheWrap(storeKey)
}

// Implements interface CacheWrapper
func (rs *Store) CacheWrapWithTrace(storeKey types.StoreKey, _ io.Writer, _ types.TraceContext) types.CacheWrap {
	return rs.CacheWrap(storeKey)
}

func (rs *Store) CacheWrapWithListeners(k types.StoreKey, listeners []types.WriteListener) types.CacheWrap {
	return rs.CacheMultiStore().CacheWrapWithListeners(k, listeners)
}

// Implements interface MultiStore
func (rs *Store) CacheMultiStore() types.CacheMultiStore {
	rs.mtx.RLock()
	defer rs.mtx.RUnlock()
	stores := make(map[types.StoreKey]types.CacheWrapper)
	for k, v := range rs.ckvStores {
		store := types.KVStore(v)
		stores[k] = store
	}
	return cachemulti.NewStore(nil, stores, rs.storeKeys, nil, nil, nil)
}

// CacheMultiStoreWithVersion Implements interface MultiStore
// used to createQueryContext, abci_query or grpc query service.
func (rs *Store) CacheMultiStoreWithVersion(version int64) (types.CacheMultiStore, error) {
	if version <= 0 || (rs.lastCommitInfo != nil && version == rs.lastCommitInfo.Version) {
		return rs.CacheMultiStore(), nil
	}
	rs.mtx.RLock()
	defer rs.mtx.RUnlock()
	stores := make(map[types.StoreKey]types.CacheWrapper)
	// add the transient/mem stores registered in current app.
	for k, store := range rs.ckvStores {
		if store.GetStoreType() != types.StoreTypeIAVL {
			stores[k] = store
		}
	}
	// TODO: May need to add historical SC store as well for nodes that doesn't enable ss but still need historical queries

	// add SS stores for historical queries
	if rs.ssStore != nil {
		for k, store := range rs.ckvStores {
			if store.GetStoreType() == types.StoreTypeIAVL {
				stores[k] = state.NewStore(rs.ssStore, k, version)
			}
		}
	}

	return cachemulti.NewStore(nil, stores, rs.storeKeys, nil, nil, nil), nil
}

// GetStore Implements interface MultiStore
func (rs *Store) GetStore(key types.StoreKey) types.Store {
	return rs.ckvStores[key]
}

// GetKVStore Implements interface MultiStore
func (rs *Store) GetKVStore(key types.StoreKey) types.KVStore {
	return rs.ckvStores[key]
}

// Implements interface MultiStore
func (rs *Store) TracingEnabled() bool {
	return false
}

// Implements interface MultiStore
func (rs *Store) SetTracer(_ io.Writer) types.MultiStore {
	return nil
}

// Implements interface MultiStore
func (rs *Store) SetTracingContext(types.TraceContext) types.MultiStore {
	return nil
}

// Implements interface Snapshotter
// not needed, memiavl manage its own snapshot/pruning strategy
func (rs *Store) PruneSnapshotHeight(_ int64) {
}

// Implements interface Snapshotter
// not needed, memiavl manage its own snapshot/pruning strategy
func (rs *Store) SetSnapshotInterval(_ uint64) {
}

// Implements interface CommitMultiStore
func (rs *Store) MountStoreWithDB(key types.StoreKey, typ types.StoreType, _ dbm.DB) {
	if key == nil {
		panic("MountIAVLStore() key cannot be nil")
	}
	if _, ok := rs.storesParams[key]; ok {
		panic(fmt.Sprintf("store duplicate store key %v", key))
	}
	if _, ok := rs.storeKeys[key.Name()]; ok {
		panic(fmt.Sprintf("store duplicate store key name %v", key))
	}
	rs.storesParams[key] = newStoreParams(key, typ)
	rs.storeKeys[key.Name()] = key
}

// Implements interface CommitMultiStore
func (rs *Store) GetCommitStore(key types.StoreKey) types.CommitStore {
	return rs.GetCommitKVStore(key)
}

// GetCommitKVStore Implements interface CommitMultiStore
func (rs *Store) GetCommitKVStore(key types.StoreKey) types.CommitKVStore {
	return rs.ckvStores[key]
}

// Implements interface CommitMultiStore
// used by normal node startup.
func (rs *Store) LoadLatestVersion() error {
	return rs.LoadVersionAndUpgrade(0, nil)
}

// Implements interface CommitMultiStore
func (rs *Store) LoadLatestVersionAndUpgrade(upgrades *types.StoreUpgrades) error {
	return rs.LoadVersionAndUpgrade(0, upgrades)
}

// Implements interface CommitMultiStore
// used by node startup with UpgradeStoreLoader
func (rs *Store) LoadVersionAndUpgrade(version int64, upgrades *types.StoreUpgrades) error {
	if version > math.MaxUint32 {
		return fmt.Errorf("version overflows uint32: %d", version)
	}

	storesKeys := make([]types.StoreKey, 0, len(rs.storesParams))
	for key := range rs.storesParams {
		storesKeys = append(storesKeys, key)
	}
	// deterministic iteration order for upgrades
	sort.Slice(storesKeys, func(i, j int) bool {
		return storesKeys[i].Name() < storesKeys[j].Name()
	})

	initialStores := make([]string, 0, len(storesKeys))
	for _, key := range storesKeys {
		if rs.storesParams[key].typ == types.StoreTypeIAVL {
			initialStores = append(initialStores, key.Name())
		}
	}
	if err := rs.scStore.Initialize(initialStores); err != nil {
		return err
	}

	var treeUpgrades []*proto.TreeNameUpgrade
	for _, key := range storesKeys {
		switch {
		case upgrades.IsDeleted(key.Name()):
			treeUpgrades = append(treeUpgrades, &proto.TreeNameUpgrade{Name: key.Name(), Delete: true})
		case upgrades.IsAdded(key.Name()) || upgrades.RenamedFrom(key.Name()) != "":
			treeUpgrades = append(treeUpgrades, &proto.TreeNameUpgrade{Name: key.Name(), RenameFrom: upgrades.RenamedFrom(key.Name())})
		}
	}

	if len(treeUpgrades) > 0 {
		if err := rs.scStore.ApplyUpgrades(treeUpgrades); err != nil {
			return err
		}
	}
	var err error
	newStores := make(map[types.StoreKey]types.CommitKVStore, len(storesKeys))
	for _, key := range storesKeys {
		newStores[key], err = rs.loadCommitStoreFromParams(key, rs.storesParams[key])
		if err != nil {
			return err
		}
	}

	rs.mtx.Lock()
	defer rs.mtx.Unlock()
	rs.ckvStores = newStores
	// to keep the root hash compatible with cosmos-sdk 0.46
	if rs.scStore.Version() != 0 {
		rs.lastCommitInfo = convertCommitInfo(rs.scStore.LastCommitInfo())
		rs.lastCommitInfo = amendCommitInfo(rs.lastCommitInfo, rs.storesParams)
	} else {
		rs.lastCommitInfo = &types.CommitInfo{}
	}
	return nil
}

func (rs *Store) loadCommitStoreFromParams(key types.StoreKey, params storeParams) (types.CommitKVStore, error) {
	switch params.typ {
	case types.StoreTypeMulti:
		panic("recursive MultiStores not yet supported")
	case types.StoreTypeIAVL:
		tree := rs.scStore.GetTreeByName(key.Name())
		if tree == nil {
			return nil, fmt.Errorf("new store is not added in upgrades: %s", key.Name())
		}
		return types.CommitKVStore(commitment.NewStore(tree, rs.logger)), nil
	case types.StoreTypeDB:
		panic("recursive MultiStores not yet supported")
	case types.StoreTypeTransient:
		_, ok := key.(*types.TransientStoreKey)
		if !ok {
			return nil, fmt.Errorf("invalid StoreKey for StoreTypeTransient: %s", key.String())
		}
		return transient.NewStore(), nil
	case types.StoreTypeMemory:
		if _, ok := key.(*types.MemoryStoreKey); !ok {
			return nil, fmt.Errorf("unexpected key type for a MemoryStoreKey; got: %s", key.String())
		}
		return mem.NewStore(), nil

	default:
		panic(fmt.Sprintf("unrecognized store type %v", params.typ))
	}
}

// Implements interface CommitMultiStore
// used by export cmd
func (rs *Store) LoadVersion(ver int64) error {
	return rs.LoadVersionAndUpgrade(ver, nil)
}

// SetInterBlockCache is a noop since we do caching on its own, which works well with zero-copy.
func (rs *Store) SetInterBlockCache(_ types.MultiStorePersistentCache) {}

// SetInitialVersion Implements interface CommitMultiStore
// used by InitChain when the initial height is bigger than 1
func (rs *Store) SetInitialVersion(version int64) error {
	return rs.scStore.SetInitialVersion(version)
}

// Implements interface CommitMultiStore
func (rs *Store) SetIAVLCacheSize(_ int) {
}

// Implements interface CommitMultiStore
func (rs *Store) SetIAVLDisableFastNode(_ bool) {
}

// Implements interface CommitMultiStore
func (rs *Store) SetLazyLoading(_ bool) {
}

// RollbackToVersion delete the versions after `target` and update the latest version.
// it should only be called in standalone cli commands.
func (rs *Store) RollbackToVersion(target int64) error {
	if target <= 0 {
		return fmt.Errorf("invalid rollback height target: %d", target)
	}

	if target > math.MaxUint32 {
		return fmt.Errorf("rollback height target %d exceeds max uint32", target)
	}
	return rs.scStore.Rollback(target)
}

// getStoreByName performs a lookup of a StoreKey given a store name typically
// provided in a path. The StoreKey is then used to perform a lookup and return
// a Store. If the Store is wrapped in an inter-block cache, it will be unwrapped
// prior to being returned. If the StoreKey does not exist, nil is returned.
func (rs *Store) GetStoreByName(name string) types.Store {
	key := rs.storeKeys[name]
	if key == nil {
		return nil
	}

	return rs.GetCommitKVStore(key)
}

// Implements interface Queryable
func (rs *Store) Query(req abci.RequestQuery) abci.ResponseQuery {
	version := req.Height
	if version <= 0 {
		version = rs.scStore.Version()
	}
	path := req.Path
	storeName, subPath, err := parsePath(path)
	if err != nil {
		return sdkerrors.QueryResult(err)
	}
	var store types.Queryable

	if !req.Prove && version < rs.lastCommitInfo.Version && rs.ssStore != nil {
		// Serve abci query from ss store if no proofs needed
		store = types.Queryable(state.NewStore(rs.ssStore, types.NewKVStoreKey(storeName), version))
	} else if version < rs.lastCommitInfo.Version {
		// Serve abci query from historical sc store if proofs needed
		scStore, err := rs.scStore.LoadVersion(version, true)
		defer scStore.Close()
		if err != nil {
			return sdkerrors.QueryResult(err)
		}
		store = types.Queryable(commitment.NewStore(scStore.GetTreeByName(storeName), rs.logger))
	} else {
		// Serve directly from latest sc store
		store = types.Queryable(commitment.NewStore(rs.scStore.GetTreeByName(storeName), rs.logger))
	}

	// trim the path and execute the query
	req.Path = subPath
	res := store.Query(req)

	if !req.Prove || !rootmulti.RequireProof(subPath) {
		return res
	}
	if res.ProofOps == nil || len(res.ProofOps.Ops) == 0 {
		return sdkerrors.QueryResult(errors.Wrap(sdkerrors.ErrInvalidRequest, "proof is unexpectedly empty; ensure height has not been pruned"))
	}
	commitInfo := convertCommitInfo(rs.scStore.LastCommitInfo())
	commitInfo = amendCommitInfo(commitInfo, rs.storesParams)
	// Restore origin path and append proof op.
	res.ProofOps.Ops = append(res.ProofOps.Ops, commitInfo.ProofOp(storeName))
	return res
}

// parsePath expects a format like /<storeName>[/<subpath>]
// Must start with /, subpath may be empty
// Returns error if it doesn't start with /
func parsePath(path string) (storeName string, subpath string, err error) {
	if !strings.HasPrefix(path, "/") {
		return storeName, subpath, errors.Wrapf(sdkerrors.ErrUnknownRequest, "invalid path: %s", path)
	}

	paths := strings.SplitN(path[1:], "/", 2)
	storeName = paths[0]

	if len(paths) == 2 {
		subpath = "/" + paths[1]
	}

	return storeName, subpath, nil
}

type storeParams struct {
	key types.StoreKey
	typ types.StoreType
}

func newStoreParams(key types.StoreKey, typ types.StoreType) storeParams {
	return storeParams{
		key: key,
		typ: typ,
	}
}

func mergeStoreInfos(commitInfo *types.CommitInfo, storeInfos []types.StoreInfo) *types.CommitInfo {
	infos := make([]types.StoreInfo, 0, len(commitInfo.StoreInfos)+len(storeInfos))
	infos = append(infos, commitInfo.StoreInfos...)
	infos = append(infos, storeInfos...)
	sort.SliceStable(infos, func(i, j int) bool {
		return infos[i].Name < infos[j].Name
	})
	return &types.CommitInfo{
		Version:    commitInfo.Version,
		StoreInfos: infos,
	}
}

// amendCommitInfo add mem stores commit infos to keep it compatible with cosmos-sdk 0.46
func amendCommitInfo(commitInfo *types.CommitInfo, storeParams map[types.StoreKey]storeParams) *types.CommitInfo {
	var extraStoreInfos []types.StoreInfo
	for key := range storeParams {
		typ := storeParams[key].typ
		if typ != types.StoreTypeIAVL && typ != types.StoreTypeTransient {
			extraStoreInfos = append(extraStoreInfos, types.StoreInfo{
				Name:     key.Name(),
				CommitId: types.CommitID{},
			})
		}
	}
	return mergeStoreInfos(commitInfo, extraStoreInfos)
}

func convertCommitInfo(commitInfo *proto.CommitInfo) *types.CommitInfo {
	storeInfos := make([]types.StoreInfo, len(commitInfo.StoreInfos))
	for i, storeInfo := range commitInfo.StoreInfos {
		storeInfos[i] = types.StoreInfo{
			Name: storeInfo.Name,
			CommitId: types.CommitID{
				Version: storeInfo.CommitId.Version,
				Hash:    storeInfo.CommitId.Hash,
			},
		}
	}
	return &types.CommitInfo{
		Version:    commitInfo.Version,
		StoreInfos: storeInfos,
	}
}

// GetWorkingHash returns the working app hash
func (rs *Store) GetWorkingHash() ([]byte, error) {
	if err := rs.flush(); err != nil {
		return nil, err
	}
	commitInfo := convertCommitInfo(rs.scStore.WorkingCommitInfo())
	// for sdk 0.46 and backward compatibility
	commitInfo = amendCommitInfo(commitInfo, rs.storesParams)
	return commitInfo.Hash(), nil
}

func (rs *Store) GetEvents() []abci.Event {
	panic("should never attempt to get events from commit multi store")
}

func (rs *Store) ResetEvents() {
	panic("should never attempt to reset events from commit multi store")
}

// ListeningEnabled will always return false for seiDB
func (rs *Store) ListeningEnabled(_ types.StoreKey) bool {
	return false
}

// AddListeners is no-opts for seiDB
func (rs *Store) AddListeners(_ types.StoreKey, _ []types.WriteListener) {
	return
}

// Restore Implements interface Snapshotter
func (rs *Store) Restore(
	height uint64, format uint32, protoReader protoio.Reader,
) (snapshottypes.SnapshotItem, error) {
	if rs.scStore != nil {
		if err := rs.scStore.Close(); err != nil {
			return snapshottypes.SnapshotItem{}, fmt.Errorf("failed to close db: %w", err)
		}
	}
	item, err := rs.restore(int64(height), protoReader)
	if err != nil {
		return snapshottypes.SnapshotItem{}, err
	}

	return item, rs.LoadLatestVersion()
}

func (rs *Store) restore(height int64, protoReader protoio.Reader) (snapshottypes.SnapshotItem, error) {
	var (
		ssImporter   chan sstypes.SnapshotNode
		snapshotItem snapshottypes.SnapshotItem
		storeKey     string
		restoreErr   error
	)
	scImporter, err := rs.scStore.Importer(height)
	if err != nil {
		return snapshottypes.SnapshotItem{}, err
	}
	if rs.ssStore != nil {
		ssImporter = make(chan sstypes.SnapshotNode, 10000)
		go func() {
			err := rs.ssStore.Import(height, ssImporter)
			if err != nil {
				panic(err)
			}
		}()
	}
loop:
	for {
		snapshotItem = snapshottypes.SnapshotItem{}
		err = protoReader.ReadMsg(&snapshotItem)
		if err == io.EOF {
			break
		} else if err != nil {
			restoreErr = errors.Wrap(err, "invalid protobuf message")
			break loop
		}

		switch item := snapshotItem.Item.(type) {
		case *snapshottypes.SnapshotItem_Store:
			storeKey = item.Store.Name
			if err = scImporter.AddTree(storeKey); err != nil {
				restoreErr = err
				break loop
			}
		case *snapshottypes.SnapshotItem_IAVL:
			if item.IAVL.Height > math.MaxInt8 {
				restoreErr = errors.Wrapf(sdkerrors.ErrLogic, "node height %v cannot exceed %v",
					item.IAVL.Height, math.MaxInt8)
				break loop
			}
			node := &sctypes.SnapshotNode{
				Key:     item.IAVL.Key,
				Value:   item.IAVL.Value,
				Height:  int8(item.IAVL.Height),
				Version: item.IAVL.Version,
			}
			// Protobuf does not differentiate between []byte{} as nil, but fortunately IAVL does
			// not allow nil keys nor nil values for leaf nodes, so we can always set them to empty.
			if node.Key == nil {
				node.Key = []byte{}
			}
			if node.Height == 0 && node.Value == nil {
				node.Value = []byte{}
			}
			scImporter.AddNode(node)

			// Check if we should also import to SS store
			if rs.ssStore != nil && node.Height == 0 && ssImporter != nil {
				ssImporter <- sstypes.SnapshotNode{
					StoreKey: storeKey,
					Key:      node.Key,
					Value:    node.Value,
				}
			}
		default:
			// unknown element, could be an extension
			break loop
		}
	}

	if err = scImporter.Close(); err != nil {
		if restoreErr == nil {
			restoreErr = err
		}
	}
	if ssImporter != nil {
		close(ssImporter)
	}

	return snapshotItem, restoreErr
}

// Snapshot Implements the interface from Snapshotter
func (rs *Store) Snapshot(height uint64, protoWriter protoio.Writer) error {
	if height > math.MaxUint32 {
		return fmt.Errorf("height overflows uint32: %d", height)
	}

	exporter, err := rs.scStore.Exporter(int64(height))
	if err != nil {
		return err
	}
	defer exporter.Close()
	for {
		item, err := exporter.Next()
		if err != nil {
			if err == commonerrors.ErrorExportDone {
				break
			}
			return err
		}

		switch item := item.(type) {
		case *sctypes.SnapshotNode:
			if err := protoWriter.WriteMsg(&snapshottypes.SnapshotItem{
				Item: &snapshottypes.SnapshotItem_IAVL{
					IAVL: &snapshottypes.SnapshotIAVLItem{
						Key:     item.Key,
						Value:   item.Value,
						Height:  int32(item.Height),
						Version: item.Version,
					},
				},
			}); err != nil {
				return err
			}
		case string:
			if err := protoWriter.WriteMsg(&snapshottypes.SnapshotItem{
				Item: &snapshottypes.SnapshotItem_Store{
					Store: &snapshottypes.SnapshotStoreItem{
						Name: item,
					},
				},
			}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown item type %T", item)
		}
	}

	return nil
}
