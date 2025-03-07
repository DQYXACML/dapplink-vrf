package txmgr

import (
	"errors"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"golang.org/x/net/context"
	"math/big"
	"strings"
	"sync"
	"time"
)

type UpdateGasPriceFunc = func(ctx context.Context) (*types.Transaction, error)

type SendTransactionFunc = func(ctx context.Context, tx *types.Transaction) error

type Config struct {
	ResubmissionTimeout       time.Duration // 重新提交交易的时间间隔
	ReceiptQueryInterval      time.Duration // 查询交易回执的时间间隔
	NumConfirmations          uint64        // 交易需要的最小确认数
	SafeAbortNonceTooLowCount uint64        // 发送交易后， nonce 值过低报错出现的次数
}

type TxManager interface {
	Send(ctx context.Context, updateGasPrice UpdateGasPriceFunc, sendTxn SendTransactionFunc) (*types.Receipt, error) // 发送并获取交易回执
}

type ReceiptSource interface {
	BlockNumber(ctx context.Context) (uint64, error)                                    // 获取当前块高
	TransactionReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) // 根据交易哈希获取交易回执
}

type SimpleTxManager struct {
	cfg     Config
	backend ReceiptSource
	l       log.Logger
}

func NewSimpleTxManager(cfg Config, backend ReceiptSource) *SimpleTxManager {
	if cfg.NumConfirmations == 0 {
		panic("txmgr: NumConfirmations must be > 0")
	}
	return &SimpleTxManager{
		cfg:     cfg,
		backend: backend,
	}
}

func (m *SimpleTxManager) Send(ctx context.Context, updateGasPrice UpdateGasPriceFunc, sendTx SendTransactionFunc) (*types.Receipt, error) {
	var wg sync.WaitGroup
	defer wg.Wait()

	ctxc, cancel := context.WithCancel(ctx)
	defer cancel()

	sendState := NewSendState(m.cfg.SafeAbortNonceTooLowCount)

	receiptChan := make(chan *types.Receipt, 1)
	sendTxAsync := func() {
		defer wg.Done()

		// 构建交易
		tx, err := updateGasPrice(ctxc) // 更新gas
		if err != nil {
			// 被取消了的话
			if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled") {
				return
			}
			log.Error("ContractsCaller update txn gas price fail", "err", err)
			cancel() // 向下传递取消
			return
		}

		txHash := tx.Hash()
		nonce := tx.Nonce()
		gasTipCap := tx.GasTipCap()
		gasFeeCap := tx.GasFeeCap()
		log.Debug("ContractsCaller publishing transaction", "txHash", txHash, "nonce", nonce, "gasTipCap", gasTipCap, "gasFeeCap", gasFeeCap)

		err = sendTx(ctxc, tx)          // 发送交易
		sendState.ProcessSendError(err) // 处理交易错误，只处理nonce问题

		if err != nil {
			if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled") {
				return
			}
			log.Error("ContractsCaller unable to publish transaction", "err", err)
			if sendState.ShouldAbortImmediately() {
				cancel()
			}
			return
		}

		log.Debug("ContractsCaller transaction published successfully", "hash", txHash, "nonce", nonce, "gasTipCap", gasTipCap, "gasFeeCap", gasFeeCap)

		receipt, err := waitMined(
			ctxc, m.backend, tx, m.cfg.ReceiptQueryInterval, m.cfg.NumConfirmations, sendState)
		if err != nil {
			log.Debug("ContractsCaller send tx failed", "hash", txHash, "nonce", nonce, "gasTipCap", gasTipCap, "gasFeeCap", gasFeeCap, "err", err)
		}
		if receipt != nil {
			select {
			case receiptChan <- receipt:
				log.Trace("ContractsCaller send tx succeeded", "hash", txHash,
					"nonce", nonce, "gasTipCap", gasTipCap,
					"gasFeeCap", gasFeeCap)
			default:
			}
		}
	}
	wg.Add(1)
	go sendTxAsync()

	ticker := time.NewTicker(m.cfg.ResubmissionTimeout)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if sendState.IsWaitingForConfirmation() {
				continue
			}
			wg.Add(1)
			go sendTxAsync()
		case <-ctxc.Done():
			return nil, ctxc.Err()
		case receipt := <-receiptChan:
			return receipt, nil
		}
	}

}

func WaitMined(
	ctx context.Context,
	backend ReceiptSource,
	tx *types.Transaction,
	queryInterval time.Duration,
	numConfirmations uint64,
) (*types.Receipt, error) {
	return waitMined(ctx, backend, tx, queryInterval, numConfirmations, nil)
}

// waitMined 查询交易回执。可参考的定时器写法
func waitMined(
	ctx context.Context,
	backend ReceiptSource,
	tx *types.Transaction,
	queryInterval time.Duration,
	numConfirmations uint64,
	sendState *SendState,
) (*types.Receipt, error) {
	queryTicker := time.NewTicker(queryInterval)
	defer queryTicker.Stop()

	txHash := tx.Hash()

	for {
		receipt, err := backend.TransactionReceipt(ctx, txHash)
		switch {
		case receipt != nil:
			if sendState != nil {
				sendState.TxMined(txHash)
			}

			txHeight := receipt.BlockNumber.Uint64()   // 收据树的块高
			tipHeight, err := backend.BlockNumber(ctx) // 最新块高
			if err != nil {
				log.Error("ContractsCaller Unable to fetch block number", "err", err)
				break
			}

			log.Trace("ContractsCaller Transaction mined, checking confirmations",
				"txHash", txHash, "txHeight", txHeight,
				"tipHeight", tipHeight,
				"numConfirmations", numConfirmations)

			// 超过numConfirmations个块的确认后，即为确认
			if txHeight+numConfirmations <= tipHeight+1 {
				log.Debug("ContractsCaller Transaction confirmed", "txHash", txHash)
				return receipt, nil
			}

			confsRemaining := (txHeight + numConfirmations) - (tipHeight + 1)
			log.Info("ContractsCaller Transaction not yet confirmed", "txHash", txHash,
				"confsRemaining", confsRemaining)
		case err != nil:
			log.Trace("ContractsCaller Receipt retrieve failed", "hash", txHash,
				"err", err)

		default:
			// 收据树为空，且没有错误，则还没出块
			if sendState != nil {
				sendState.TxNotMined(txHash)
			}
			log.Trace("ContractsCaller Transaction not yet mined", "hash", txHash)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-queryTicker.C:
		}
	}
}

func CalcGasFeeCap(baseFee, gasTipCap *big.Int) *big.Int {
	return new(big.Int).Add(
		gasTipCap,
		new(big.Int).Mul(baseFee, big.NewInt(2)),
	)
}
