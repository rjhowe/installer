package asset

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	stateFileName = ".openshift_install_state.json"
)

// Store is a store for the states of assets.
type Store interface {
	// Fetch retrieves the state of the given asset, generating it and its
	// dependencies if necessary.
	Fetch(Asset) error

	// Save dumps the entire state map into a file
	Save(dir string) error

	// Purge deletes the on-disk assets that are consumed already.
	// E.g., install-config.yml will be deleted after fetching 'manifests'.
	Purge(excluded []WritableAsset) error
}

// assetState includes an asset and a boolean that indicates
// whether it's dirty or not.
type assetState struct {
	asset Asset
	dirty bool
}

// StoreImpl is the implementation of Store.
type StoreImpl struct {
	directory       string
	assets          map[reflect.Type]assetState
	stateFileAssets map[string]json.RawMessage
	fileFetcher     *fileFetcher
	onDiskAssets    []WritableAsset // This records the on-disk assets that are loaded already, which will be cleaned up in the end.
}

// NewStore returns an asset store that implements the Store interface.
func NewStore(dir string) (Store, error) {
	store := &StoreImpl{
		directory:   dir,
		fileFetcher: &fileFetcher{directory: dir},
		assets:      make(map[reflect.Type]assetState),
	}

	if err := store.load(dir); err != nil {
		return nil, err
	}

	return store, nil
}

// Fetch retrieves the state of the given asset, generating it and its
// dependencies if necessary.
func (s *StoreImpl) Fetch(asset Asset) error {
	_, err := s.fetch(asset, "")
	return err
}

// load retrieves the state from the state file present in the given directory
// and returns the assets map
func (s *StoreImpl) load(dir string) error {
	path := filepath.Join(dir, stateFileName)
	assets := make(map[string]json.RawMessage)
	data, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	err = json.Unmarshal(data, &assets)
	if err != nil {
		return errors.Wrapf(err, "failed to unmarshal state file %q", path)
	}
	s.stateFileAssets = assets
	return nil
}

// LoadAssetFromState renders the asset object arguments from the state file contents.
func (s *StoreImpl) LoadAssetFromState(asset Asset) error {
	bytes, ok := s.stateFileAssets[reflect.TypeOf(asset).String()]
	if !ok {
		return errors.Errorf("asset %q is not found in the state file", asset.Name())
	}
	return json.Unmarshal(bytes, asset)
}

// IsAssetInState tests whether the asset is in the state file.
func (s *StoreImpl) IsAssetInState(asset Asset) bool {
	_, ok := s.stateFileAssets[reflect.TypeOf(asset).String()]
	return ok
}

// Save dumps the entire state map into a file
func (s *StoreImpl) Save(dir string) error {
	assetMap := make(map[string]Asset)
	for k, v := range s.assets {
		assetMap[k.String()] = v.asset
	}
	data, err := json.MarshalIndent(&assetMap, "", "    ")
	if err != nil {
		return err
	}

	path := filepath.Join(dir, stateFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := ioutil.WriteFile(path, data, 0644); err != nil {
		return err
	}
	return nil
}

// fetch populates the given asset, generating it and its dependencies if
// necessary, and returns whether or not the asset had to be regenerated and
// any errors.
func (s *StoreImpl) fetch(asset Asset, indent string) (bool, error) {
	logrus.Debugf("%sFetching %q...", indent, asset.Name())

	// Return immediately if the asset is found in the cache,
	// this is because we are doing a depth-first-search, it's guaranteed
	// that we always fetch the parent before children, so we don't need
	// to worry about invalidating anything in the cache.
	storedAsset, ok := s.assets[reflect.TypeOf(asset)]
	if ok {
		logrus.Debugf("%sReusing previously-fetched %q", indent, asset.Name())
		reflect.ValueOf(asset).Elem().Set(reflect.ValueOf(storedAsset.asset).Elem())
		return storedAsset.dirty, nil
	}

	dependencies := asset.Dependencies()
	parents := make(Parents, len(dependencies))
	if len(dependencies) > 0 {
		logrus.Debugf("%sFetching dependencies of %q...", indent, asset.Name())
	}

	var anyParentsDirty bool
	for _, d := range dependencies {
		dirty, err := s.fetch(d, indent+"  ")
		if err != nil {
			return false, errors.Wrapf(err, "failed to fetch dependency of %q", asset.Name())
		}
		if dirty {
			anyParentsDirty = true
		}
		parents.Add(d)
	}

	// Try to find the asset from the state file.
	foundInStateFile := s.IsAssetInState(asset)
	if foundInStateFile {
		logrus.Debugf("%sFound %q in state file", indent, asset.Name())
	}

	// Try to load from the provided files.
	var foundOnDisk bool
	if as, ok := asset.(WritableAsset); ok {
		var err error
		foundOnDisk, err = as.Load(s.fileFetcher)
		if err != nil {
			return false, errors.Wrapf(err, "failed to load asset %q", asset.Name())
		}
		if foundOnDisk {
			logrus.Infof("Consuming %q from target directory", asset.Name())
			s.onDiskAssets = append(s.onDiskAssets, as)
		}
	}

	if anyParentsDirty && foundOnDisk {
		logrus.Warningf("%sDiscarding the %q that was provided in the target directory because its dependencies are dirty and it needs to be regenerated", indent, asset.Name())
	}

	if anyParentsDirty || (!foundOnDisk && !foundInStateFile) {
		logrus.Debugf("%sGenerating %q...", indent, asset.Name())
		if err := asset.Generate(parents); err != nil {
			return false, errors.Wrapf(err, "failed to generate asset %q", asset.Name())
		}
	} else if foundInStateFile && foundOnDisk {
		logrus.Debugf("%sLoading %q from both state file and target directory", indent, asset.Name())

		stateAsset := reflect.New(reflect.TypeOf(asset).Elem()).Interface().(Asset)
		if err := s.LoadAssetFromState(stateAsset); err != nil {
			return false, errors.Wrapf(err, "failed to load asset %q from state file", asset.Name())
		}

		// If the on-disk asset is the same as the one in the state file, there
		// is no need to consider the one on disk and to mark the asset dirty.
		if reflect.DeepEqual(stateAsset, asset) {
			foundOnDisk = false
		}
	} else if foundInStateFile {
		logrus.Debugf("%sLoading %q from state file", indent, asset.Name())
		if err := s.LoadAssetFromState(asset); err != nil {
			return false, errors.Wrapf(err, "failed to load asset %q from state file", asset.Name())
		}
	} else if foundOnDisk {
		logrus.Debugf("%sLoading %q from target directory", indent, asset.Name())
	}

	dirty := anyParentsDirty || foundOnDisk
	s.assets[reflect.TypeOf(asset)] = assetState{asset: asset, dirty: dirty}
	return dirty, nil
}

// Purge deletes the on-disk assets that are consumed already.
// E.g., install-config.yml will be deleted after fetching 'manifests'.
// The target assets are excluded.
func (s *StoreImpl) Purge(excluded []WritableAsset) error {
	var toPurge []WritableAsset

	for _, asset := range s.onDiskAssets {
		var found bool
		for _, as := range excluded {
			if reflect.TypeOf(as) == reflect.TypeOf(asset) {
				found = true
				break
			}
		}
		if !found {
			toPurge = append(toPurge, asset)
		}
	}

	for _, asset := range toPurge {
		logrus.Debugf("Purging asset %q", asset.Name())
		for _, f := range asset.Files() {
			if err := os.Remove(filepath.Join(s.directory, f.Filename)); err != nil {
				return errors.Wrapf(err, "failed to remove file %q", f.Filename)
			}
		}
	}
	return nil
}
