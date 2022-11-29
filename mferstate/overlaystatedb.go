package mferstate

import (
	"bufio"
	"bytes"
	"context"
	"log"
	"math/big"
	"os"

	"github.com/davecgh/go-spew/spew"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/kataras/golog"
	"github.com/sec-bit/mfer-node/constant"
	"github.com/sec-bit/mfer-node/utils"
)

type OverrideAccount struct {
	Nonce     *hexutil.Uint64              `json:"nonce,omitempty"`
	Code      *hexutil.Bytes               `json:"code,omitempty"`
	Balance   **hexutil.Big                `json:"balance,omitempty"`
	State     *map[common.Hash]common.Hash `json:"state,omitempty"`
	StateDiff *map[common.Hash]common.Hash `json:"stateDiff,omitempty"`
}

// StateOverride is the collection of overridden accounts.
type StateOverride map[common.Address]*OverrideAccount
type OverlayStateDB struct {
	ctx           context.Context
	ec            *rpc.Client
	conn          *ethclient.Client
	cacheFilePath string
	maxKeyCache   uint64
	refundGas     uint64
	state         *OverlayState
	stateBN       *uint64
}

func (db *OverlayStateDB) GetOverlayDepth() int64 {
	return db.state.deriveCnt
}

func NewOverlayStateDB(rpcClient *rpc.Client, blockNumber *uint64, keyCacheFilePath string, maxKeyCache uint64, batchSize int) (db *OverlayStateDB) {
	db = &OverlayStateDB{
		ctx:           context.Background(),
		ec:            rpcClient,
		conn:          ethclient.NewClient(rpcClient),
		cacheFilePath: keyCacheFilePath,
		maxKeyCache:   maxKeyCache,
		refundGas:     0,
		stateBN:       blockNumber,
	}
	state := NewOverlayState(db.ctx, db.ec, db.stateBN, batchSize).Derive("protect underlying") // protect underlying state
	db.state = state
	return db
}

func (db *OverlayStateDB) resetScratchPad(clearKeyCache bool) {
	s := db.state
	s.scratchPadMutex.Lock()
	golog.Debug("[reset scratchpad] lock scratchPad")
	defer func() {
		golog.Debug("[reset scratchpad] unlock scratchPad")
		s.scratchPadMutex.Unlock()
	}()

	f, err := os.OpenFile(db.cacheFilePath, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		log.Panicf("openfile error: %v", err)
	}
	defer f.Close()

	if clearKeyCache {
		s.scratchPad = make(map[string][]byte)
		s.accessedAccounts = make(map[common.Address]bool)
		f.Truncate(0)
		f.Seek(0, 0)
		return
	}

	golog.Debug("[reset scratchpad] load cached scratchPad key")
	scanner := bufio.NewScanner(f)
	keyCacheCnt := uint64(0)
	for scanner.Scan() {
		if keyCacheCnt >= db.maxKeyCache {
			break
		}
		txt := scanner.Text()
		s.scratchPad[string(STATE_KEY.Bytes())+string(common.Hex2Bytes(txt))] = []byte{}
		if scanner.Err() != nil {
			break
		}
		keyCacheCnt++
	}

	cachedStr := ""
	reqs := make([]*StorageReq, 0)
	for key := range s.scratchPad {
		keyBytes := []byte(key)
		if len(keyBytes) == 32+20+32 && common.BytesToHash(keyBytes[:32]) == STATE_KEY {
			acc := common.BytesToAddress(keyBytes[32 : 32+20])
			s.accessedAccountsMutex.Lock()
			s.accessedAccounts[acc] = true
			s.accessedAccountsMutex.Unlock()
			key := common.BytesToHash(keyBytes[32+20:])
			reqs = append(reqs, &StorageReq{Address: acc, Key: key})
			cachedStr += (common.Bytes2Hex(keyBytes[32:]) + "\n")
			if err != nil {
				golog.Errorf("write string err: %v", err)
			}
		}
	}

	err = s.loadStateBatchRPC(reqs)
	if err != nil {
		log.Panic(err)
	}
	f.Truncate(0)
	f.Seek(0, 0)
	f.WriteString(cachedStr)
	golog.Infof("cache saved @ %s", db.cacheFilePath)

	for _, result := range reqs {
		stateKey := calcStateKey(result.Address, result.Key)
		s.scratchPad[stateKey] = result.Value[:]
	}

	golog.Infof("[reset scratchpad] state prefetch done, slot num: %d", len(s.scratchPad))
	golog.Infof("[reset scratchpad] prefetching %d accounts", len(s.accessedAccounts))
	accounts := make([]common.Address, 0)
	s.accessedAccountsMutex.RLock()
	for k := range s.accessedAccounts {
		accounts = append(accounts, k)
	}
	s.accessedAccountsMutex.RUnlock()

	accountResults, err := s.loadAccountBatchRPC(accounts)
	if err != nil {
		golog.Errorf("loadAccountBatchRPC failed: %v", err)
		return
	}
	for i := range accountResults {
		nonce := uint64(accountResults[i].Nonce)
		balance := accountResults[i].Balance.ToInt()
		codeHash := accountResults[i].CodeHash
		s.scratchPad[calcKey(BALANCE_KEY, accounts[i])] = balance.Bytes()
		s.scratchPad[calcKey(NONCE_KEY, accounts[i])] = big.NewInt(int64(nonce)).Bytes()
		s.scratchPad[calcKey(CODE_KEY, accounts[i])] = accountResults[i].Code
		s.scratchPad[calcKey(CODEHASH_KEY, accounts[i])] = codeHash.Bytes()
	}
	golog.Info("[reset scratchpad] account prefetch done")
}

func (db *OverlayStateDB) InitState(fetchNewState, clearCache bool) {
	utils.PrintMemUsage("[before init]")
	reason := "reset and protect underlying"
	db.state = db.state.getRootState()
	golog.Infof("Resetting Scratchpad... BN: %d", *db.stateBN)
	if fetchNewState {
		db.resetScratchPad(clearCache)
	}
	golog.Info(reason)
	db.state = db.state.Derive(reason)
	utils.PrintMemUsage("[current]")
}

func (db *OverlayStateDB) CreateAccount(account common.Address) {}

func (db *OverlayStateDB) SubBalance(account common.Address, delta *big.Int) {
	bal, err := db.state.get(account, GET_BALANCE, common.Hash{})
	if err != nil {
		log.Panic(err)
	}
	balB := new(big.Int).SetBytes(bal)
	post := balB.Sub(balB, delta)
	db.state.scratchPad[calcKey(BALANCE_KEY, account)] = post.Bytes()
}

func (db *OverlayStateDB) AddBalance(account common.Address, delta *big.Int) {
	bal, err := db.state.get(account, GET_BALANCE, common.Hash{})
	if err != nil {
		log.Panic(err)
	}
	balB := new(big.Int).SetBytes(bal)
	post := balB.Add(balB, delta)
	db.state.scratchPad[calcKey(BALANCE_KEY, account)] = post.Bytes()
}

func (db *OverlayStateDB) InitFakeAccounts() {
	db.AddBalance(constant.FAKE_ACCOUNT_0, new(big.Int).Mul(big.NewInt(1e18), big.NewInt(1000)))
	db.AddBalance(constant.FAKE_ACCOUNT_1, new(big.Int).Mul(big.NewInt(1e18), big.NewInt(1000)))
	db.AddBalance(constant.FAKE_ACCOUNT_2, new(big.Int).Mul(big.NewInt(1e18), big.NewInt(1000)))
	db.AddBalance(constant.FAKE_ACCOUNT_3, new(big.Int).Mul(big.NewInt(1e18), big.NewInt(1000)))
	db.AddBalance(constant.FAKE_ACCOUNT_RICH, new(big.Int).Mul(big.NewInt(1e18), big.NewInt(1_000_000_000)))
}

func (db *OverlayStateDB) GetBalance(account common.Address) *big.Int {
	bal, err := db.state.get(account, GET_BALANCE, common.Hash{})
	if err != nil {
		log.Panic(err)
	}
	balB := new(big.Int).SetBytes(bal)
	return balB
}

func (db *OverlayStateDB) SetBalance(account common.Address, balance *big.Int) {
	db.state.scratchPad[calcKey(BALANCE_KEY, account)] = balance.Bytes()
}

func (db *OverlayStateDB) GetNonce(account common.Address) uint64 {
	nonce, err := db.state.get(account, GET_NONCE, common.Hash{})
	if err != nil {
		log.Panic(err)
	}
	nonceB := new(big.Int).SetBytes(nonce)
	return nonceB.Uint64()
}
func (db *OverlayStateDB) SetNonce(account common.Address, nonce uint64) {
	db.state.scratchPad[calcKey(NONCE_KEY, account)] = big.NewInt(int64(nonce)).Bytes()
}

func (db *OverlayStateDB) GetCodeHash(account common.Address) common.Hash {
	codehash, err := db.state.get(account, GET_CODEHASH, common.Hash{})
	if err != nil {
		log.Panic(err)
	}
	return common.BytesToHash(codehash)
}

func (db *OverlayStateDB) SetCodeHash(account common.Address, codeHash common.Hash) {
	db.state.scratchPad[calcKey(CODEHASH_KEY, account)] = codeHash.Bytes()
	if account.Hex() != (common.Address{}).Hex() {
		// log.Printf("SetCodeHash[depth:%d]: acc: %s key: %s, codehash: %s", db.state.deriveCnt, account.Hex(), calcKey( CODEHASH_KEY).Hex(), codeHash.Hex())
	}
}

func (db *OverlayStateDB) GetCode(account common.Address) []byte {
	code, err := db.state.get(account, GET_CODE, common.Hash{})
	if err != nil {
		log.Panic(err)
	}
	return code
}

func (db *OverlayStateDB) SetCode(account common.Address, code []byte) {
	db.state.scratchPad[calcKey(CODE_KEY, account)] = code
}

func (db *OverlayStateDB) GetCodeSize(account common.Address) int {
	code, err := db.state.get(account, GET_CODE, common.Hash{})
	if err != nil {
		log.Panic(err)
	}
	return len(code)
}

func (db *OverlayStateDB) AddRefund(delta uint64) { db.refundGas += delta }
func (db *OverlayStateDB) SubRefund(delta uint64) { db.refundGas -= delta }
func (db *OverlayStateDB) GetRefund() uint64      { return db.refundGas }

func (db *OverlayStateDB) GetCommittedState(account common.Address, key common.Hash) common.Hash {
	val, err := db.state.get(account, GET_STATE, key)
	if err != nil {
		log.Panic(err)
	}
	return common.BytesToHash(val)
}

func (db *OverlayStateDB) GetState(account common.Address, key common.Hash) common.Hash {
	v := db.GetCommittedState(account, key)
	// log.Printf("[R depth:%d, stateID:%02x] Acc: %s K: %s V: %s", db.state.deriveCnt, db.state.stateID, account.Hex(), key.Hex(), v.Hex())
	// log.Printf("Fetched: %s [%s] = %s", account.Hex(), key.Hex(), v.Hex())
	return v
}

func (db *OverlayStateDB) SetState(account common.Address, key common.Hash, value common.Hash) {
	// log.Printf("[W depth:%d stateID:%02x] Acc: %s K: %s V: %s", db.state.deriveCnt, db.state.stateID, account.Hex(), key.Hex(), value.Hex())
	db.state.scratchPad[calcStateKey(account, key)] = value.Bytes()
}

func (db *OverlayStateDB) Suicide(account common.Address) bool {
	db.state.scratchPad[calcKey(SUICIDE_KEY, account)] = []byte{0x01}
	return true
}

func (db *OverlayStateDB) HasSuicided(account common.Address) bool {
	if val, ok := db.state.scratchPad[calcKey(SUICIDE_KEY, account)]; ok {
		return bytes.Equal(val, []byte{0x01})
	}
	return false
}

func (db *OverlayStateDB) Exist(account common.Address) bool {
	return !db.Empty(account)
}

func (db *OverlayStateDB) Empty(account common.Address) bool {
	code := db.GetCode(account)
	nonce := db.GetNonce(account)
	balance := db.GetBalance(account)
	if len(code) == 0 && nonce == 0 && balance.Sign() == 0 {
		return true
	}
	return false
}

func (db *OverlayStateDB) PrepareAccessList(sender common.Address, dest *common.Address, precompiles []common.Address, txAccesses types.AccessList) {
}

func (db *OverlayStateDB) AddressInAccessList(addr common.Address) bool { return true }

func (db *OverlayStateDB) SlotInAccessList(addr common.Address, slot common.Hash) (addressOk bool, slotOk bool) {
	return true, true
}

func (db *OverlayStateDB) AddAddressToAccessList(addr common.Address) { return }

func (db *OverlayStateDB) AddSlotToAccessList(addr common.Address, slot common.Hash) { return }

func (db *OverlayStateDB) RevertToSnapshot(revisionID int) {
	tmpState := db.state.Parent()
	golog.Debugf("Rollbacking... revision: %d, currentID: %d", revisionID, tmpState.deriveCnt)
	for {
		if tmpState.deriveCnt+1 == int64(revisionID) {
			db.state = tmpState
			break
		} else {
			tmpState = tmpState.Parent()
		}
	}
}

func (db *OverlayStateDB) Snapshot() int {
	newOverlayState := db.state.Derive("snapshot")
	db.state = newOverlayState
	revisionID := int(newOverlayState.deriveCnt)
	return revisionID
}

func (db *OverlayStateDB) MergeTo(revisionID int) {
	currState, parentState := db.state, db.state.parent
	golog.Infof("Merging... target revisionID: %d, currentID: %d", revisionID, currState.deriveCnt)
	for {
		if currState.deriveCnt == int64(revisionID) {
			db.state = currState
			break
		}
		for k, v := range currState.scratchPad {
			parentState.scratchPad[k] = v
		}
		currState, parentState = parentState, parentState.parent
	}
}

func (db *OverlayStateDB) Clone() *OverlayStateDB {
	cpy := &OverlayStateDB{
		ctx:  db.ctx,
		ec:   db.ec,
		conn: db.conn,
		// block:     db.block,
		refundGas: 0,
		state:     db.state.Derive("clone"),
	}
	return cpy
}

func (db *OverlayStateDB) CloneFromRoot() *OverlayStateDB {
	cpy := &OverlayStateDB{
		ctx:       db.ctx,
		ec:        db.ec,
		conn:      db.conn,
		refundGas: 0,
		state:     db.state.DeriveFromRoot(),
	}
	return cpy
}

func (db *OverlayStateDB) CacheSize() (size int) {
	if db.state == nil {
		return -1
	}
	root := db.state.getRootState()
	root.scratchPadMutex.RLock()
	defer root.scratchPadMutex.RUnlock()
	for k, v := range root.scratchPad {
		size += (len(k) + len(v))
	}
	return size
}

func (db *OverlayStateDB) RPCRequestCount() (cnt int64) {
	if db.state == nil {
		return -1
	}
	return db.state.getRootState().rpcCnt
}

func (db *OverlayStateDB) StateBlockNumber() (cnt uint64) {
	return *db.stateBN
}

func (db *OverlayStateDB) AddLog(vLog *types.Log) {
	golog.Debugf("StateID: %02x, AddLog: %s", db.state.stateID, spew.Sdump(vLog))
	db.state.txLogs[db.state.currentTxHash] = append(db.state.txLogs[db.state.currentTxHash], vLog)
}

func (db *OverlayStateDB) GetLogs(txHash common.Hash) []*types.Log {
	tmpStateDB := db.state
	logs := make([]*types.Log, 0)
	for {
		if tmpStateDB == nil {
			break
		}
		if tmpStateDB.txLogs[txHash] != nil {
			golog.Debugf("StateID: %02x, GetLogs: %s", db.state.stateID, spew.Sdump(tmpStateDB.txLogs[txHash]))
			logs = append(tmpStateDB.txLogs[txHash], logs...)
		}
		tmpStateDB = tmpStateDB.parent
	}
	return logs
}

func (db *OverlayStateDB) AddReceipt(txHash common.Hash, receipt *types.Receipt) {
	golog.Debugf("StateID: %02x, AddReceipt: %s", db.state.stateID, spew.Sdump(receipt))
	db.state.receipts[txHash] = receipt
}

func (db *OverlayStateDB) GetReceipt(txHash common.Hash) *types.Receipt {
	tmpStateDB := db.state
	for {
		if tmpStateDB.parent == nil {
			return nil
		}
		if receipt, ok := tmpStateDB.receipts[txHash]; ok {
			receipt.Logs = db.GetLogs(txHash)
			return receipt
		}
		tmpStateDB = tmpStateDB.parent
	}
}

func (db *OverlayStateDB) AddPreimage(common.Hash, []byte) {}

func (db *OverlayStateDB) ForEachStorage(account common.Address, callback func(common.Hash, common.Hash) bool) error {
	return nil
}

func (db *OverlayStateDB) StartLogCollection(txHash, blockHash common.Hash) {
	db.state.currentTxHash = txHash
	db.state.currentBlockHash = blockHash
}

func (db *OverlayStateDB) SetBatchSize(batchSize int) {
	db.state.getRootState().batchSize = batchSize
}

func (db *OverlayStateDB) getMergedScratchPad() map[string][]byte {
	mergedScratchPad := make(map[string][]byte)
	tmpState := db.state
	for {
		if tmpState.parent == nil {
			break
		}

		for k, v := range tmpState.scratchPad {
			if _, ok := mergedScratchPad[k]; ok {
				continue
			}
			mergedScratchPad[k] = v
		}
		tmpState = tmpState.parent
	}
	return mergedScratchPad
}

func (s *OverlayStateDB) GetStateDiff() StateOverride {
	mergedScratchPad := s.getMergedScratchPad()
	accounts := make(StateOverride)
	clonedState := s.CloneFromRoot()
	clonedState.state.scratchPad = mergedScratchPad
	for k := range mergedScratchPad {
		key := common.BytesToHash([]byte(k)[:32])
		account := common.BytesToAddress([]byte(k)[32 : 32+20])
		var override *OverrideAccount
		if _override, ok := accounts[account]; !ok {
			tmp := &OverrideAccount{}
			accounts[account] = tmp
			override = tmp
		} else {
			override = _override
		}
		switch key {
		case BALANCE_KEY:
			balance := (*hexutil.Big)(clonedState.GetBalance(account))
			override.Balance = &balance
		case NONCE_KEY:
			nonce := clonedState.GetNonce(account)
			override.Nonce = (*hexutil.Uint64)(&nonce)
		case CODE_KEY:
			code := clonedState.GetCode(account)
			override.Code = (*hexutil.Bytes)(&code)
			// scratchpadKey = calcKey(CODE_KEY, account)
		case STATE_KEY:
			stateKey := common.BytesToHash([]byte(k)[32+20 : 32+20+32])
			stateValue := clonedState.GetState(account, stateKey)
			if override.StateDiff == nil {
				stateDiff := make(map[common.Hash]common.Hash)
				override.StateDiff = &stateDiff
			}
			(*override.StateDiff)[stateKey] = stateValue
			// scratchpadKey = calcStateKey(account, key)
		}
	}
	return accounts
}
