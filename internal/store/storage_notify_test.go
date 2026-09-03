package store

import (
	"testing"
	"time"
)

func TestStoreChangeBroker(t *testing.T) {
	store, err := OpenStore(t.TempDir() + "/proxy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	id, changes := store.SubscribeChanges()
	defer store.UnsubscribeChanges(id)

	store.NotifyChange()
	store.NotifyChange()
	store.NotifyChange()
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("no change event received after NotifyChange")
	}
	select {
	case <-changes:
		t.Fatal("burst of notifies should coalesce into one pending event")
	case <-time.After(50 * time.Millisecond):
	}

	store.UnsubscribeChanges(id)
	if _, open := <-changes; open {
		t.Fatal("unsubscribe should close the channel")
	}

	store.NotifyChange()
}
