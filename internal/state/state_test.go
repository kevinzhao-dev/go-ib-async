package state

import (
	"errors"
	"sync"
	"testing"
)

func TestRequestMapStartComplete(t *testing.T) {
	rm := NewRequestMap()
	ch := rm.Start(1)

	go func() {
		rm.Complete(1, Result{Value: "hello", Err: nil})
	}()

	result := <-ch
	if result.Value != "hello" || result.Err != nil {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestRequestMapCancel(t *testing.T) {
	rm := NewRequestMap()
	ch := rm.Start(1)

	rm.Cancel(1)

	_, ok := <-ch
	if ok {
		t.Fatal("channel should be closed after cancel")
	}
}

func TestRequestMapHas(t *testing.T) {
	rm := NewRequestMap()
	if rm.Has(1) {
		t.Fatal("should not have 1")
	}
	rm.Start(1)
	if !rm.Has(1) {
		t.Fatal("should have 1")
	}
}

func TestRequestMapLen(t *testing.T) {
	rm := NewRequestMap()
	rm.Start(1)
	rm.Start(2)
	if rm.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", rm.Len())
	}
}

func TestRequestMapDrainAll(t *testing.T) {
	rm := NewRequestMap()
	ch1 := rm.Start(1)
	ch2 := rm.Start(2)

	testErr := errors.New("disconnected")
	rm.DrainAll(testErr)

	r1 := <-ch1
	r2 := <-ch2
	if r1.Err != testErr || r2.Err != testErr {
		t.Fatal("DrainAll should send error to all pending")
	}
	if rm.Len() != 0 {
		t.Fatal("should be empty after drain")
	}
}

func TestRequestMapConcurrent(t *testing.T) {
	rm := NewRequestMap()
	var wg sync.WaitGroup

	for i := range 100 {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			ch := rm.Start(id)
			go rm.Complete(id, Result{Value: id})
			result := <-ch
			if result.Value != id {
				t.Errorf("id %d: got %v", id, result.Value)
			}
		}(int64(i))
	}
	wg.Wait()
}

func TestTickMapsComplete(t *testing.T) {
	allTypes := AllTickTypes()
	if len(allTypes) < 60 {
		t.Fatalf("expected at least 60 tick types, got %d", len(allTypes))
	}

	// Verify no duplicate keys within the same map
	checkNoDuplicates := func(name string, m map[int]string) {
		// maps can't have duplicate keys in Go, so just check non-empty
		if len(m) == 0 {
			t.Fatalf("%s is empty", name)
		}
	}
	checkNoDuplicates("PriceTickMap", PriceTickMap)
	checkNoDuplicates("SizeTickMap", SizeTickMap)
	checkNoDuplicates("GenericTickMap", GenericTickMap)
	checkNoDuplicates("GreeksTickMap", GreeksTickMap)
	checkNoDuplicates("EfpTickMap", EfpTickMap)
	checkNoDuplicates("StringTickMap", StringTickMap)
	checkNoDuplicates("TimestampTickMap", TimestampTickMap)
	checkNoDuplicates("RTVolumeTickMap", RTVolumeTickMap)
}

func TestTickMapValues(t *testing.T) {
	// Spot check critical mappings
	tests := []struct {
		tickType int
		mapName  string
		m        map[int]string
		want     string
	}{
		{6, "PriceTickMap", PriceTickMap, "High"},
		{7, "PriceTickMap", PriceTickMap, "Low"},
		{9, "PriceTickMap", PriceTickMap, "Close"},
		{14, "PriceTickMap", PriceTickMap, "Open"},
		{37, "PriceTickMap", PriceTickMap, "MarkPrice"},
		{8, "SizeTickMap", SizeTickMap, "Volume"},
		{21, "SizeTickMap", SizeTickMap, "AvVolume"},
		{23, "GenericTickMap", GenericTickMap, "HistVolatility"},
		{24, "GenericTickMap", GenericTickMap, "ImpliedVolatility"},
		{49, "GenericTickMap", GenericTickMap, "Halted"},
		{10, "GreeksTickMap", GreeksTickMap, "BidGreeks"},
		{13, "GreeksTickMap", GreeksTickMap, "ModelGreeks"},
	}

	for _, tt := range tests {
		got, ok := tt.m[tt.tickType]
		if !ok {
			t.Fatalf("%s[%d] not found", tt.mapName, tt.tickType)
		}
		if got != tt.want {
			t.Fatalf("%s[%d] = %q, want %q", tt.mapName, tt.tickType, got, tt.want)
		}
	}
}

func TestManagerStartEndReq(t *testing.T) {
	m := NewManager()
	ch := m.StartReq(42, nil)

	m.AppendResult(42, "item1")
	m.AppendResult(42, "item2")
	m.EndReq(42, nil, nil)

	result := <-ch
	if result.Err != nil {
		t.Fatalf("unexpected error: %v", result.Err)
	}

	items := result.Value.([]interface{})
	if len(items) != 2 || items[0] != "item1" || items[1] != "item2" {
		t.Fatalf("unexpected result: %v", items)
	}
}

func TestManagerReset(t *testing.T) {
	m := NewManager()
	m.Accounts = []string{"DU123456"}
	m.Reset()
	if len(m.Accounts) != 0 {
		t.Fatal("Reset should clear accounts")
	}
}
