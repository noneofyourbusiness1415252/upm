package internal

import (
	"encoding/json"
	"io/ioutil"
	"os"
	"path/filepath"
)

func getStoreLocation() string {
	loc, ok := os.LookupEnv("UPM_STORE")
	if ok {
		return loc
	} else {
		return ".upm/store.json"
	}
}

func readStore() store {
	filename := getStoreLocation()
	bytes, err := ioutil.ReadFile(filename)

	if err != nil {
		if os.IsNotExist(err) {
			return store{}
		}
		die("%s: %s", filename, err)
	}

	var store store
	err = json.Unmarshal(bytes, &store)

	if err != nil {
		die("%s: %s", filename, err)
	}

	return store
}

func writeStore(store store) {
	filename := getStoreLocation()

	filename, err := filepath.Abs(filename)
	if err != nil {
		die("%s: %s", filename, err)
	}

	directory, _ := filepath.Split(filename)
	if err := os.MkdirAll(directory, 0777); err != nil {
		die("%s: %s", directory, err)
	}

	bytes, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		panicf("writeStore: json.MarshallIndent failed", err)
	}
	if err := ioutil.WriteFile(filename, bytes, 0666); err != nil {
		die("%s: %s", filename, err)
	}
}

func updateStoreHashes(specfile string, lockfile string) {
	store := readStore()
	store.specfileHash = hashFile(specfile)
	store.lockfileHash = hashFile(lockfile)

	if store.specfileHash == "" {
		die("file does not exist: %s", specfile)
	}

	if store.lockfileHash == "" {
		die("file does not exist: %s", lockfile)
	}

	writeStore(store)
}
