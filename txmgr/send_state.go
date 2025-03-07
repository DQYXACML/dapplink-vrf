package txmgr

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"strings"
	"sync"
)

type SendState struct {
	minedTxs         map[common.Hash]struct{}
	nonceTooLowCount uint64
	mu               sync.RWMutex

	safeAbortNonceTooLowCount uint64
}

func NewSendState(safeAbortNonceTooLowCount uint64) *SendState {
	if safeAbortNonceTooLowCount == 0 {
		panic("txmgr: safeAbortNonceTooLowCount must be > 0")
	}
	return &SendState{
		minedTxs:                  make(map[common.Hash]struct{}),
		nonceTooLowCount:          0,
		safeAbortNonceTooLowCount: safeAbortNonceTooLowCount,
	}
}

// ProcessSendError 处理交易出错，如果是nonce过低，那么记录一下错误次数
func (s *SendState) ProcessSendError(err error) {
	if err == nil {
		return
	}

	if !strings.Contains(err.Error(), core.ErrNonceTooLow.Error()) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.nonceTooLowCount++
}

func (s *SendState) TxMined(txHash common.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.minedTxs[txHash] = struct{}{}
}

func (s *SendState) TxNotMined(txHash common.Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, wasMined := s.minedTxs[txHash]
	delete(s.minedTxs, txHash)

	if len(s.minedTxs) == 0 && wasMined {
		s.nonceTooLowCount = 0
	}
}

// ShouldAbortImmediately nonce过低的交易数超过阈值，停止发送交易
func (s *SendState) ShouldAbortImmediately() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.minedTxs) > 0 {
		return false
	}

	return s.nonceTooLowCount >= s.safeAbortNonceTooLowCount
}

func (s *SendState) IsWaitingForConfirmation() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.minedTxs) > 0
}
