// Copyright (c) 2020 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package relayer

import (
	"context"
	"crypto/ecdsa"
	"log"
	"math/big"
	"reflect"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/iotexproject/ioTube/witness-service/contract"
	"github.com/iotexproject/ioTube/witness-service/util"
	"github.com/iotexproject/iotex-address/address"
	"github.com/iotexproject/iotex-antenna-go/v2/iotex"
	"github.com/iotexproject/iotex-proto/golang/iotexapi"
	"github.com/iotexproject/iotex-proto/golang/iotextypes"
)

// transferValidatorOnIoTeX defines the transfer validator
type transferValidatorOnIoTeX struct {
	mu       sync.RWMutex
	gasLimit uint64
	gasPrice *big.Int

	privateKey            *ecdsa.PrivateKey
	relayerAddr           address.Address
	validatorContractAddr address.Address

	client                 iotex.AuthedClient
	validatorContract      iotex.Contract
	validatorContractABI   abi.ABI
	witnessListContract    iotex.Contract
	witnessListContractABI abi.ABI
	witnesses              map[string]bool
}

// NewTransferValidatorOnIoTeX creates a new TransferValidator on IoTeX
func NewTransferValidatorOnIoTeX(
	client iotex.AuthedClient,
	privateKey *ecdsa.PrivateKey,
	validatorContractAddr address.Address,
) (TransferValidator, error) {
	validatorContractIoAddr, err := address.FromBytes(validatorContractAddr.Bytes())
	if err != nil {
		return nil, err
	}
	validatorABI, err := abi.JSON(strings.NewReader(contract.TransferValidatorABI))
	if err != nil {
		return nil, err
	}
	validatorContract := client.Contract(validatorContractAddr, validatorABI)

	data, err := validatorContract.Read("witnessList").Call(context.Background())
	if err != nil {
		return nil, err
	}

	ret, err := validatorABI.Unpack("witnessList", data.Raw)
	if err != nil {
		return nil, err
	}
	witnessContractAddr, ok := ret[0].(common.Address)
	if !ok {
		return nil, errors.Errorf("invalid type %s", reflect.TypeOf(ret[0]))
	}
	witnessContractIoAddr, err := address.FromBytes(witnessContractAddr.Bytes())
	if err != nil {
		return nil, err
	}
	witnessContractABI, err := abi.JSON(strings.NewReader(contract.AddressListABI))
	if err != nil {
		return nil, err
	}
	relayerAddr, err := address.FromBytes(crypto.PubkeyToAddress(privateKey.PublicKey).Bytes())
	if err != nil {
		return nil, err
	}

	return &transferValidatorOnIoTeX{
		gasLimit: 2000000,
		gasPrice: big.NewInt(1000000000000),

		privateKey:            privateKey,
		relayerAddr:           relayerAddr,
		validatorContractAddr: validatorContractIoAddr,

		client:                 client,
		validatorContract:      validatorContract,
		validatorContractABI:   validatorABI,
		witnessListContract:    client.Contract(witnessContractIoAddr, witnessContractABI),
		witnessListContractABI: witnessContractABI,
	}, nil
}

func (tv *transferValidatorOnIoTeX) Address() common.Address {
	tv.mu.RLock()
	defer tv.mu.RUnlock()

	return common.BytesToAddress(tv.validatorContractAddr.Bytes())
}

func (tv *transferValidatorOnIoTeX) refresh() error {
	witnesses := []common.Address{}
	countData, err := tv.witnessListContract.Read("count").Call(context.Background())
	if err != nil {
		return err
	}
	ret, err := countData.Unmarshal()
	if err != nil {
		return err
	}
	count, ok := ret[0].(*big.Int)
	if !ok {
		return errors.Errorf("invalid type %s", reflect.TypeOf(ret[0]))
	}
	offset := big.NewInt(0)
	limit := uint8(10)
	for offset.Cmp(count) < 0 {
		data, err := tv.witnessListContract.Read("getActiveItems", offset, limit).Call(context.Background())
		if err != nil {
			return err
		}
		ret, err := tv.witnessListContractABI.Unpack("getActiveItems", data.Raw)
		if err != nil {
			return err
		}
		ai, ok := ret[0].(struct {
			Count *big.Int
			Items []common.Address
		})
		if !ok {
			return errors.Errorf("invalid type %s", reflect.TypeOf(ret[0]))
		}

		witnesses = append(witnesses, ai.Items[:int(ai.Count.Int64())]...)
		offset.Add(offset, big.NewInt(int64(limit)))
	}
	log.Println("refresh Witnesses on IoTeX")
	activeWitnesses := make(map[string]bool)
	for _, w := range witnesses {
		addr, err := address.FromBytes(w.Bytes())
		if err != nil {
			return err
		}
		log.Println("\t" + addr.String())
		activeWitnesses[w.Hex()] = true
	}
	tv.witnesses = activeWitnesses

	return nil
}

func (tv *transferValidatorOnIoTeX) isActiveWitness(witness common.Address) bool {
	val, ok := tv.witnesses[witness.Hex()]

	return ok && val
}

// Check returns true if a transfer has been settled
func (tv *transferValidatorOnIoTeX) Check(transfer *Transfer) (StatusOnChainType, error) {
	tv.mu.RLock()
	defer tv.mu.RUnlock()
	settleHeightData, err := tv.validatorContract.Read("settles", transfer.id).Call(context.Background())
	if err != nil {
		return StatusOnChainUnknown, err
	}
	ret, err := tv.validatorContractABI.Unpack("settles", settleHeightData.Raw)
	if err != nil {
		return StatusOnChainUnknown, err
	}
	settleHeight, ok := ret[0].(*big.Int)
	if !ok {
		return StatusOnChainUnknown, errors.Errorf("invalid type %s", reflect.TypeOf(ret[0]))
	}

	if settleHeight.Cmp(big.NewInt(0)) > 0 {
		// TODO: send 0.1 iotx
		/*
			addr, err := address.FromBytes(transfer.recipient.Bytes())
			if err != nil {
				log.Panic("failed to convert address", transfer.recipient)
			}
			_, err = tv.client.Transfer(addr, math.BigPow(10, 17)).SetGasPrice(tv.gasPrice).SetGasLimit(10000).Call(context.Background())
			if err != nil {
				log.Print("failed to transfer iotx", err)
			}
		*/
		return StatusOnChainSettled, nil
	}
	response, err := tv.client.API().GetReceiptByAction(context.Background(), &iotexapi.GetReceiptByActionRequest{})
	switch status.Code(err) {
	case codes.NotFound:
		return StatusOnChainNeedSpeedUp, nil
	case codes.OK:
		break
	default:
		return StatusOnChainUnknown, err
	}
	if response != nil {
		// no matter what the receipt status is, mark the validation as failure
		return StatusOnChainRejected, nil
	}

	return StatusOnChainNotConfirmed, nil
}

func (tv *transferValidatorOnIoTeX) submit(transfer *Transfer, witnesses []*Witness, resubmit bool) (common.Hash, uint64, *big.Int, error) {
	if err := tv.refresh(); err != nil {
		return common.Hash{}, 0, nil, errors.Wrap(errNoncritical, err.Error())
	}
	signatures := []byte{}
	numOfValidSignatures := 0
	for _, witness := range witnesses {
		if !tv.isActiveWitness(witness.addr) {
			addr, err := address.FromBytes(witness.addr.Bytes())
			if err != nil {
				return common.Hash{}, 0, nil, errors.Wrap(errNoncritical, err.Error())
			}
			log.Printf("witness %s is inactive\n", addr.String())
			continue
		}
		signatures = append(signatures, witness.signature...)
		numOfValidSignatures++
	}
	if numOfValidSignatures*3 <= len(tv.witnesses)*2 {
		return common.Hash{}, 0, nil, errInsufficientWitnesses
	}
	accountMeta, err := tv.relayerAccountMeta()
	if err != nil {
		return common.Hash{}, 0, nil, errors.Wrapf(errNoncritical, "failed to get account of %s, %v", tv.relayerAddr.String(), err)
	}
	balance, ok := big.NewInt(0).SetString(accountMeta.Balance, 10)
	if !ok {
		return common.Hash{}, 0, nil, errors.Wrapf(errNoncritical, "failed to convert balance %s of account %s, %v", accountMeta.Balance, tv.relayerAddr.String(), err)
	}
	if balance.Cmp(new(big.Int).Mul(tv.gasPrice, new(big.Int).SetUint64(tv.gasLimit))) < 0 {
		util.Alert("IOTX native balance has dropped to " + balance.String() + ", please refill account for gas " + tv.relayerAddr.String())
	}
	var nonce uint64
	if resubmit {
		nonce = transfer.nonce
	} else {
		nonce = accountMeta.Nonce + 1
	}

	actionHash, err := tv.validatorContract.Execute(
		"submit",
		transfer.cashier,
		transfer.token,
		new(big.Int).SetUint64(transfer.index),
		transfer.sender,
		transfer.recipient,
		transfer.amount,
		signatures,
	).SetGasPrice(tv.gasPrice).
		SetGasLimit(tv.gasLimit).
		SetNonce(nonce).
		Call(context.Background())
	if err != nil {
		return common.Hash{}, 0, nil, err
	}

	return common.BytesToHash(actionHash[:]), nonce, tv.gasPrice, nil
}

// Submit submits validation for a transfer
func (tv *transferValidatorOnIoTeX) Submit(transfer *Transfer, witnesses []*Witness) (common.Hash, uint64, *big.Int, error) {
	tv.mu.Lock()
	defer tv.mu.Unlock()

	return tv.submit(transfer, witnesses, false)
}

func (tv *transferValidatorOnIoTeX) SpeedUp(transfer *Transfer, witnesses []*Witness) (common.Hash, uint64, *big.Int, error) {
	tv.mu.Lock()
	defer tv.mu.Unlock()

	return tv.submit(transfer, witnesses, true)
}

func (tv *transferValidatorOnIoTeX) relayerAccountMeta() (*iotextypes.AccountMeta, error) {
	response, err := tv.client.API().GetAccount(context.Background(), &iotexapi.GetAccountRequest{
		Address: tv.relayerAddr.String(),
	})
	if err != nil {
		return nil, err
	}
	return response.AccountMeta, nil
}
