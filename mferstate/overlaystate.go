package mferstate

import (
	"context"
	"fmt"
	"math/big"
	"math/rand"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/kataras/golog"
	"github.com/tj/go-spin"
)

type AccountResult struct {
	Address      common.Address  `json:"address"`
	AccountProof []string        `json:"accountProof"`
	Balance      *hexutil.Big    `json:"balance"`
	CodeHash     common.Hash     `json:"codeHash"`
	Nonce        hexutil.Uint64  `json:"nonce"`
	StorageHash  common.Hash     `json:"storageHash"`
	StorageProof []StorageResult `json:"storageProof"`
}
type StorageResult struct {
	Key   string       `json:"key"`
	Value *hexutil.Big `json:"value"`
	Proof []string     `json:"proof"`
}

type StorageReq struct {
	Address common.Address
	Key     common.Hash
	Value   common.Hash
	Error   error
}

func (r *StorageReq) Hash() common.Hash {
	return crypto.Keccak256Hash(r.Address.Bytes(), r.Key.Bytes())
}

type OverlayState struct {
	ctx    context.Context
	ec     *rpc.Client
	conn   *ethclient.Client
	parent *OverlayState
	bn     *uint64
	// lastBN          *uint64
	scratchPadMutex *sync.RWMutex
	scratchPad      map[string][]byte
	batchSize       int

	accessedAccountsMutex *sync.RWMutex
	accessedAccounts      map[common.Address]bool

	txLogs                          map[common.Hash][]*types.Log
	receipts                        map[common.Hash]*types.Receipt
	currentTxHash, currentBlockHash common.Hash
	deriveCnt                       int64
	rpcCnt                          int64
	storageReqChan                  chan chan StorageReq
	accReqChan                      chan chan FetchedAccountResult

	loadAccountMutex *sync.Mutex

	upstreamReqCh chan bool
	clientReqCh   chan bool

	reason  string
	stateID uint64
}

func NewOverlayState(ctx context.Context, ec *rpc.Client, bn *uint64, batchSize int) *OverlayState {
	state := &OverlayState{
		ctx:             ctx,
		ec:              ec,
		conn:            ethclient.NewClient(ec),
		parent:          nil,
		bn:              bn,
		scratchPadMutex: &sync.RWMutex{},
		scratchPad:      make(map[string][]byte),
		batchSize:       batchSize,

		accessedAccountsMutex: &sync.RWMutex{},
		accessedAccounts:      make(map[common.Address]bool),

		txLogs:           make(map[common.Hash][]*types.Log),
		receipts:         make(map[common.Hash]*types.Receipt),
		deriveCnt:        0,
		storageReqChan:   make(chan chan StorageReq, 500),
		accReqChan:       make(chan chan FetchedAccountResult, 200),
		loadAccountMutex: &sync.Mutex{},

		upstreamReqCh: make(chan bool, 100),
		clientReqCh:   make(chan bool, 100),
	}
	go state.timeSlot()
	return state
}

func (s *OverlayState) Derive(reason string) *OverlayState {
	state := &OverlayState{
		parent:           s,
		scratchPad:       make(map[string][]byte),
		txLogs:           make(map[common.Hash][]*types.Log),
		receipts:         make(map[common.Hash]*types.Receipt),
		deriveCnt:        s.deriveCnt + 1,
		currentTxHash:    s.currentTxHash,
		currentBlockHash: s.currentBlockHash,

		stateID: rand.Uint64(),
		reason:  reason,
	}
	golog.Debugf("derive reason: %s from: %02x, id: %02x, depth: %d", reason, s.stateID, state.stateID, state.deriveCnt)
	return state
}

func (s *OverlayState) Parent() *OverlayState {
	// s.scratchPad = make(map[string][]byte)
	golog.Debugf("poping id: %02x, reason: %s", s.stateID, s.reason)
	// close(s.shouldStop)
	return s.parent
}

type RequestType int

const (
	GET_BALANCE RequestType = iota
	GET_NONCE
	GET_CODE
	GET_CODEHASH
	GET_STATE
)

var (
	BALANCE_KEY  = crypto.Keccak256Hash([]byte("mfersafe-scratchpad-balance"))
	NONCE_KEY    = crypto.Keccak256Hash([]byte("mfersafe-scratchpad-nonce"))
	CODE_KEY     = crypto.Keccak256Hash([]byte("mfersafe-scratchpad-code"))
	CODEHASH_KEY = crypto.Keccak256Hash([]byte("mfersafe-scratchpad-codehash"))
	STATE_KEY    = crypto.Keccak256Hash([]byte("mfersafe-scratchpad-state"))
	SUICIDE_KEY  = crypto.Keccak256Hash([]byte("mfersafe-suicide-state"))
)

type FetchedAccountResult struct {
	Account  common.Address
	Balance  hexutil.Big
	CodeHash common.Hash
	Nonce    hexutil.Uint64
	Code     hexutil.Bytes
}

func (s *OverlayState) loadAccountBatchRPC(accounts []common.Address) ([]FetchedAccountResult, error) {
	rpcTries := 0
	bn := big.NewInt(int64(*s.bn))
	hexBN := hexutil.EncodeBig(bn)

	result := make([]FetchedAccountResult, len(accounts))
	batchElem := make([]rpc.BatchElem, len(accounts)*3)

	for i, account := range accounts {
		getNonceReq := rpc.BatchElem{
			Method: "eth_getTransactionCount",
			Args:   []interface{}{account, hexBN},
			Result: &result[i].Nonce,
		}

		getBalanceReq := rpc.BatchElem{
			Method: "eth_getBalance",
			Args:   []interface{}{account, hexBN},
			Result: &result[i].Balance,
		}

		getCodeReq := rpc.BatchElem{
			Method: "eth_getCode",
			Args:   []interface{}{account, hexBN},
			Result: &result[i].Code,
		}
		batchElem[i*3] = getNonceReq
		batchElem[i*3+1] = getBalanceReq
		batchElem[i*3+2] = getCodeReq

		s.accessedAccountsMutex.Lock()
		s.accessedAccounts[account] = true
		s.accessedAccountsMutex.Unlock()
	}

	step := s.batchSize
	start := time.Now()
	for begin := 0; begin < len(batchElem); begin += step {
		for {
			// s.upstreamReqCh <- true
			end := begin + step
			if end > len(batchElem) {
				end = len(batchElem)
			}
			golog.Debugf("loadAccount batch req(total=%d): begin: %d, end: %d", len(batchElem), begin, end)
			err := s.ec.BatchCallContext(s.ctx, batchElem[begin:end])
			if err != nil {
				rpcTries++
				if rpcTries > 5 {
					return nil, err
				} else {
					golog.Warn("retrying loadAccountSimple")
					time.Sleep(100 * time.Millisecond)
					continue
				}
			}
			break
		}
	}

	for i := range accounts {
		if len(result[i].Code) == 0 {
			result[i].CodeHash = common.Hash{}
		} else {
			result[i].CodeHash = crypto.Keccak256Hash(result[i].Code)
		}
	}
	golog.Debugf("fetched %d accounts batched@%d (consumes: %v)", len(accounts), *s.bn, time.Since(start))

	return result, nil
}

func (s *OverlayState) loadAccountViaGetProof(account common.Address) (*AccountResult, []byte, error) {
	var result AccountResult
	var code hexutil.Bytes
	rpcTries := 0
	hexBN := hexutil.EncodeBig(big.NewInt(int64(*s.bn)))

	getProofReq := rpc.BatchElem{
		Method: "eth_getProof",
		Args:   []interface{}{account, []string{}, hexBN},
		Result: &result,
	}

	getCodeReq := rpc.BatchElem{
		Method: "eth_getCode",
		Args:   []interface{}{account, hexBN},
		Result: &code,
	}

	for {
		start := time.Now()
		err := s.ec.BatchCallContext(s.ctx, []rpc.BatchElem{getProofReq, getCodeReq})
		if err != nil {
			rpcTries++
			if rpcTries > 5 {
				return nil, nil, err
			} else {
				golog.Warn("retrying getProof")
				time.Sleep(100 * time.Millisecond)
				continue
			}
		} else {
			rpcTries = 0
			if getProofReq.Error != nil {
				golog.Errorf("getProof err: %v", getProofReq)
			}
			if getCodeReq.Error != nil {
				golog.Errorf("getProof err: %v", getCodeReq)
			}
			golog.Infof("fetched account batched@%d {proof, code}: %s (consumes: %v)", *s.bn, account.Hex(), time.Since(start))
			break
		}
	}

	return &result, code, nil
}

func (s *OverlayState) loadStateBatchRPC(storageReqs []*StorageReq) error {
	// TODO: dedup

	s.rpcCnt++
	// s.upstreamReqCh <- true
	reqs := make([]rpc.BatchElem, len(storageReqs))
	values := make([]common.Hash, len(storageReqs))
	bn := big.NewInt(int64(*s.bn))
	hexBN := hexutil.EncodeBig(bn)
	for i := range reqs {
		reqs[i] = rpc.BatchElem{
			Method: "eth_getStorageAt",
			Args:   []interface{}{storageReqs[i].Address, storageReqs[i].Key, hexBN},
			Result: &values[i],
		}
	}

	step := s.batchSize
	start := time.Now()
	for begin := 0; begin < len(reqs); begin += step {
		end := begin + step
		if end > len(reqs) {
			end = len(reqs)
		}
		golog.Debugf("loadState batch req(total=%d): begin: %d, end: %d", len(reqs), begin, end)
		if err := s.ec.BatchCallContext(s.ctx, reqs[begin:end]); err != nil {
			return err
		}
	}

	golog.Debugf("fetched %d state batched@%d (consumes: %v)", len(reqs), *s.bn, time.Since(start))

	for i := range storageReqs {
		storageReqs[i].Value = values[i]
	}
	return nil
}

func (s *OverlayState) loadStateRPC(account common.Address, key common.Hash) (common.Hash, error) {
	s.rpcCnt++
	// s.upstreamReqCh <- true
	storage, err := s.conn.StorageAt(s.ctx, account, key, big.NewInt(int64(*s.bn)))
	if err != nil {
		return common.Hash{}, err
	}
	value := common.BytesToHash(storage)
	return value, nil
}

func (s *OverlayState) timeSlot() {
	tickerStorage := time.NewTicker(time.Millisecond * 3)
	tickerAccount := time.NewTicker(time.Millisecond * 10)
	for {
		storageReqLen := len(s.storageReqChan)
		accReqLen := len(s.accReqChan)
		select {
		case <-tickerStorage.C:
			storageReqPending := make([]*StorageReq, storageReqLen)
			storageReqChanPending := make([]chan StorageReq, storageReqLen)
			for i := 0; i < storageReqLen; i++ {
				req := <-s.storageReqChan
				storageReq := <-req
				storageReqPending[i] = &storageReq
				storageReqChanPending[i] = req
			}
			if storageReqLen > 0 {
				for {
					err := s.loadStateBatchRPC(storageReqPending)
					if err != nil {
						golog.Errorf("loadStateBatch, err: %v", err)
						time.Sleep(time.Second * 1)
					} else {
						break
					}
				}
			}

			for i := 0; i < storageReqLen; i++ {
				req := storageReqChanPending[i]
				req <- *storageReqPending[i]
				close(req)
			}
		case <-tickerAccount.C:
			accReqPending := make([]*FetchedAccountResult, accReqLen)
			accReqChanPending := make([]chan FetchedAccountResult, accReqLen)
			accounts := make([]common.Address, accReqLen)
			for i := 0; i < accReqLen; i++ {
				req := <-s.accReqChan
				accReq := <-req
				accReqPending[i] = &accReq
				accReqChanPending[i] = req
				accounts[i] = accReq.Account
			}

			var accResult []FetchedAccountResult
			var err error
			if accReqLen > 0 {
				for {
					accResult, err = s.loadAccountBatchRPC(accounts)
					if err != nil {
						golog.Errorf("loadAccountBatchRPC, err: %v", err)
						time.Sleep(time.Second * 1)
					} else {
						break
					}
				}
			}

			for i := 0; i < len(accResult); i++ {
				req := accReqChanPending[i]
				req <- accResult[i]
				close(req)
			}
		}
	}
}

func (db *OverlayState) statistics() {
	spinnerUpstream := &spin.Spinner{}
	spinnerUpstream.Set("🌍🌎🌏")
	spinnerClient := &spin.Spinner{}
	spinnerClient.Set("▁▂▃▄▅▆▇█▇▆▅▄▃▁")

	upstreamReqCnt := 0
	clientReqCnt := 0
	upstreamSpinStr := "-"
	clientSpinStr := "-"
	statisticsStr := "\nUpstream %s  [%d]\tDownstream %s  [%d]"

	ticker := time.NewTicker(time.Second)
	for {
		fmt.Print("\033[G\033[K")
		select {
		case <-db.upstreamReqCh:
			upstreamReqCnt++
			upstreamSpinStr = spinnerUpstream.Next()
			fmt.Printf(statisticsStr, upstreamSpinStr, upstreamReqCnt, clientSpinStr, clientReqCnt)
		case <-db.clientReqCh:
			clientReqCnt++
			clientSpinStr = spinnerClient.Next()
			fmt.Printf(statisticsStr, upstreamSpinStr, upstreamReqCnt, clientSpinStr, clientReqCnt)
		case <-ticker.C:
			upstreamSpinStr = spinnerUpstream.Next()
			clientSpinStr = spinnerClient.Next()
			fmt.Printf(statisticsStr, upstreamSpinStr, upstreamReqCnt, clientSpinStr, clientReqCnt)
		}
		fmt.Printf("\033[A")
	}
}

func (s *OverlayState) loadState(account common.Address, key common.Hash) common.Hash {
	retChan := make(chan StorageReq)
	s.storageReqChan <- retChan
	retChan <- StorageReq{Address: account, Key: key}
	result := <-retChan
	// spew.Dump(result)
	return result.Value
}

func (s *OverlayState) loadAccount(account common.Address) FetchedAccountResult {
	retChan := make(chan FetchedAccountResult)
	s.accReqChan <- retChan
	retChan <- FetchedAccountResult{Account: account}
	result := <-retChan
	// spew.Dump(result)
	return result
}

func calcKey(op common.Hash, account common.Address) string {
	return string(append(op.Bytes(), account.Bytes()...))
}

func calcStateKey(account common.Address, key common.Hash) string {
	getStateKey := calcKey(STATE_KEY, account)
	stateKey := getStateKey + string(key.Bytes())
	return stateKey
}

func (s *OverlayState) get(account common.Address, action RequestType, key common.Hash) ([]byte, error) {
	// if s.parent == nil && *s.bn != *s.lastBN {
	// 	golog.Infof("State BN: %d", *s.bn)
	// 	s.lastBN = *s.bn
	// }
	var scratchpadKey string
	switch action {
	case GET_BALANCE:
		scratchpadKey = calcKey(BALANCE_KEY, account)
	case GET_NONCE:
		scratchpadKey = calcKey(NONCE_KEY, account)
	case GET_CODE:
		scratchpadKey = calcKey(CODE_KEY, account)
	case GET_CODEHASH:
		scratchpadKey = calcKey(CODEHASH_KEY, account)
	case GET_STATE:
		scratchpadKey = calcStateKey(account, key)
	}

	if s.parent == nil {
		s.scratchPadMutex.Lock()
		if val, ok := s.scratchPad[scratchpadKey]; ok {
			s.scratchPadMutex.Unlock()
			return val, nil
		}
		s.scratchPadMutex.Unlock()

		var res []byte
		switch action {
		case GET_STATE:
			result := s.loadState(account, key)
			s.scratchPadMutex.Lock()
			s.scratchPad[scratchpadKey] = result.Bytes()
			s.scratchPadMutex.Unlock()
			res = result.Bytes()

		case GET_BALANCE, GET_NONCE, GET_CODE, GET_CODEHASH:
			result := s.loadAccount(account)
			nonce := uint64(result.Nonce)
			balance := result.Balance.ToInt()
			codeHash := result.CodeHash

			s.scratchPadMutex.Lock()
			if _, ok := s.scratchPad[calcKey(BALANCE_KEY, account)]; !ok {
				s.scratchPad[calcKey(BALANCE_KEY, account)] = balance.Bytes()
			}
			if _, ok := s.scratchPad[calcKey(NONCE_KEY, account)]; !ok {
				s.scratchPad[calcKey(NONCE_KEY, account)] = big.NewInt(int64(nonce)).Bytes()
			}
			if _, ok := s.scratchPad[calcKey(CODE_KEY, account)]; !ok {
				s.scratchPad[calcKey(CODE_KEY, account)] = result.Code
			}
			if _, ok := s.scratchPad[calcKey(CODEHASH_KEY, account)]; !ok {
				s.scratchPad[calcKey(CODEHASH_KEY, account)] = codeHash.Bytes()
			}

			switch action {
			case GET_BALANCE:
				res = s.scratchPad[calcKey(BALANCE_KEY, account)]
			case GET_NONCE:
				res = s.scratchPad[calcKey(NONCE_KEY, account)]
			case GET_CODE:
				res = s.scratchPad[calcKey(CODE_KEY, account)]
			case GET_CODEHASH:
				res = s.scratchPad[calcKey(CODEHASH_KEY, account)]
			}
			s.scratchPadMutex.Unlock()
		}
		return res, nil

	} else {
		if val, ok := s.scratchPad[scratchpadKey]; ok {
			return val, nil
		}
		return s.parent.get(account, action, key)
	}
}

func (s *OverlayState) getRootState() *OverlayState {
	tmpState := s
	for {
		if tmpState.parent == nil {
			return tmpState
		} else {
			tmpState = tmpState.parent
		}
	}
}

func (s *OverlayState) DeriveFromRoot() *OverlayState {
	return s.getRootState().Derive("from root")
}
