package evmaux

import (
	"encoding/binary"
	"fmt"

	"github.com/gogo/protobuf/proto"
	"github.com/loomnetwork/go-loom/plugin/types"
	"github.com/loomnetwork/loomchain/eth/bloom"
	"github.com/loomnetwork/loomchain/log"
	"github.com/pkg/errors"
)

var (
	ErrTxReceiptNotFound      = errors.New("Tx receipt not found")
	ErrPendingReceiptNotFound = errors.New("Pending receipt not found")
)

const (
	statusTxSuccess = int32(1)
)

func (s *EvmAuxStore) GetReceipt(txHash []byte) (types.EvmTxReceipt, error) {
	txReceiptProto := s.db.Get(txHash)
	if len(txReceiptProto) == 0 {
		return types.EvmTxReceipt{}, ErrTxReceiptNotFound
	}
	txReceipt := types.EvmTxReceiptListItem{}
	err := proto.Unmarshal(txReceiptProto, &txReceipt)
	if err != nil {
		return types.EvmTxReceipt{}, err
	}
	return *txReceipt.Receipt, nil
}

func (s *EvmAuxStore) CommitReceipts(receipts []*types.EvmTxReceipt, height uint64) error {
	if len(receipts) == 0 || s.maxReceipts == 0 {
		return nil
	}

	s.Rollback()
	size, headHash, tailHash, err := s.getDBParams()
	if err != nil {
		return errors.Wrap(err, "getting db params.")
	}

	tailReceiptItem := types.EvmTxReceiptListItem{}
	if len(headHash) > 0 {
		tailItemProto := s.db.Get(tailHash)
		if len(tailItemProto) == 0 {
			return errors.Wrap(err, "cannot find tail")
		}
		if err = proto.Unmarshal(tailItemProto, &tailReceiptItem); err != nil {
			return errors.Wrap(err, "unmarshalling tail")
		}
	}

	var txHashArray [][]byte
	events := make([]*types.EventData, 0, len(receipts))
	for _, txReceipt := range receipts {
		if txReceipt == nil || len(txReceipt.TxHash) == 0 {
			continue
		}

		// Update previous tail to point to current receipt
		if len(headHash) == 0 {
			headHash = txReceipt.TxHash
		} else {
			tailReceiptItem.NextTxHash = txReceipt.TxHash
			protoTail, err := proto.Marshal(&tailReceiptItem)
			if err != nil {
				log.Error(fmt.Sprintf("commit block receipts: marshal receipt item: %s", err.Error()))
				continue
			}
			s.batch.Set(tailHash, protoTail)
			if !s.db.Has(tailHash) {
				size++
			}

		}

		// Set current receipt as next tail
		tailHash = txReceipt.TxHash
		tailReceiptItem = types.EvmTxReceiptListItem{Receipt: txReceipt, NextTxHash: nil}

		// only upload hashes to app db if transaction successful
		if txReceipt.Status == statusTxSuccess {
			txHashArray = append(txHashArray, txReceipt.TxHash)
		}

		events = append(events, txReceipt.Logs...)
	}

	if len(tailHash) > 0 {
		protoTail, err := proto.Marshal(&tailReceiptItem)
		if err != nil {
			log.Error(fmt.Sprintf("commit block receipts: marshal receipt item: %s", err.Error()))
		} else {
			s.batch.Set(tailHash, protoTail)
			if !s.db.Has(tailHash) {
				size++
			}
		}
	}

	// clear old receipts if the number of receipts exceeds the limit
	if s.maxReceipts < size {
		var numDeleted uint64
		headHash, numDeleted, err = s.removeOldEntries(headHash, size-s.maxReceipts)
		if err != nil {
			return errors.Wrap(err, "removing old receipts")
		}
		if size < numDeleted {
			return errors.Wrap(err, "invalid count of deleted receipts")
		}
		size -= numDeleted
	}
	s.setDBParams(size, headHash, tailHash)

	filter := bloom.GenBloomFilter(events)
	if err := s.setTxHashList(txHashArray, height); err != nil {
		return errors.Wrap(err, "append tx list")
	}
	s.setBloomFilter(filter, height)

	s.Commit()
	return nil
}

func (s *EvmAuxStore) getDBParams() (size uint64, head, tail []byte, err error) {
	notEmpty := s.db.Has(currentDbSizeKey)
	if !notEmpty {
		return 0, []byte{}, []byte{}, nil
	}

	sizeB := s.db.Get(currentDbSizeKey)
	size = binary.LittleEndian.Uint64(sizeB)
	if size == 0 {
		return 0, []byte{}, []byte{}, nil
	}

	head = s.db.Get(headKey)
	if len(head) == 0 {
		return 0, []byte{}, []byte{}, errors.New("no head for non zero size receipt db")
	}

	tail = s.db.Get(tailKey)
	if len(tail) == 0 {
		return 0, []byte{}, []byte{}, errors.New("no tail for non zero size receipt db")
	}

	return size, head, tail, nil
}

func (s *EvmAuxStore) setDBParams(size uint64, head, tail []byte) {
	s.batch.Set(headKey, head)
	s.batch.Set(tailKey, tail)
	sizeB := make([]byte, 8)
	binary.LittleEndian.PutUint64(sizeB, size)
	s.batch.Set(currentDbSizeKey, sizeB)
}

func (s *EvmAuxStore) removeOldEntries(head []byte, number uint64) ([]byte, uint64, error) {
	itemsDeleted := uint64(0)
	for i := uint64(0); i < number && len(head) > 0; i++ {
		headItem := s.db.Get(head)
		txHeadReceiptItem := types.EvmTxReceiptListItem{}
		if err := proto.Unmarshal(headItem, &txHeadReceiptItem); err != nil {
			return head, itemsDeleted, errors.Wrapf(err, "unmarshal head %s", string(headItem))
		}
		s.batch.Delete(head)
		itemsDeleted++
		head = txHeadReceiptItem.NextTxHash
	}
	if itemsDeleted < number {
		return head, itemsDeleted, errors.Errorf("Unable to delete %v receipts, only %v deleted", number, itemsDeleted)
	}

	return head, itemsDeleted, nil
}
