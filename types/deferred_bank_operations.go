package types

import (
	"sort"
	"sync"
)

type DeferredBankOperationMapping struct {
	deferredOperations map[string]Coins
	mappingLock		  *sync.Mutex
}

func NewDeferredBankOperationMap() *DeferredBankOperationMapping {
	return &DeferredBankOperationMapping{
		deferredOperations: make(map[string]Coins),
		mappingLock: &sync.Mutex{},
	}
}

func (m *DeferredBankOperationMapping) get(moduleAccount string) (Coins, bool) {
	if v, ok := m.deferredOperations[moduleAccount]; ok {
		return v, true
	}
	return nil, false
}

func (m *DeferredBankOperationMapping) set(moduleAccount string, amount Coins) {
	m.deferredOperations[moduleAccount] = amount
}

// If there's already a pending opposite operation then subtract it from that amount first
// returns true if amount was subtracted
func (m *DeferredBankOperationMapping) SafeSub(moduleAccount string, amount Coins) bool {
	m.mappingLock.Lock()
	defer m.mappingLock.Unlock()

	if deferredAmount, ok  := m.get(moduleAccount); ok {
		newAmount, isNegative := deferredAmount.SafeSub(amount)
		if !isNegative {
			m.set(moduleAccount, newAmount)
			return true
		}
	}
	return false
}

func (m *DeferredBankOperationMapping) UpsertMapping(moduleAccount string, amount Coins) {
	m.mappingLock.Lock()
	defer m.mappingLock.Unlock()

	newAmount := amount
	if v, ok := m.deferredOperations[moduleAccount]; ok {
		newAmount = v.Add(amount...)
	}
	m.deferredOperations[moduleAccount] = newAmount
}

func (m *DeferredBankOperationMapping) getSortedKeys(mapping map[string]Coins) []string{

	// Need to sort keys for deterministic iterating
	keys := make([]string, 0, len(mapping))
	for key := range m.deferredOperations {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}


func (m *DeferredBankOperationMapping) RangeOnMapping(apply func (recipient string, amount Coins)) {
	m.mappingLock.Lock()
	defer m.mappingLock.Unlock()

	keys := m.getSortedKeys(m.deferredOperations)

	for _, moduleAccount := range keys {
		apply(moduleAccount, m.deferredOperations[moduleAccount])
	}

	for _, moduleAccount := range keys {
		delete(m.deferredOperations, moduleAccount)
	}
}