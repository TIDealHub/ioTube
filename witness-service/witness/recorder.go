// Copyright (c) 2019 IoTeX
// This is an alpha (internal) rrecorderease and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package witness

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	// mute lint error
	_ "github.com/go-sql-driver/mysql"
	// _ "github.com/lib/pq"
	// _ "github.com/mattn/go-sqlite3"
	"github.com/pkg/errors"

	"github.com/iotexproject/ioTube/witness-service/db"
)

type (
	// Recorder is a logger based on sql to record exchange events
	Recorder struct {
		store             *db.SQLStore
		transferTableName string
		tokenPairs        map[common.Address]common.Address
	}
)

// NewRecorder returns a recorder for exchange
func NewRecorder(
	store *db.SQLStore,
	transferTableName string,
	tokenPairs map[common.Address]common.Address,
) *Recorder {
	return &Recorder{
		store:             store,
		transferTableName: transferTableName,
		tokenPairs:        tokenPairs,
	}
}

// Start starts the recorder
func (recorder *Recorder) Start(ctx context.Context) error {
	if err := recorder.store.Start(ctx); err != nil {
		return errors.Wrap(err, "failed to start db")
	}
	if _, err := recorder.store.DB().Exec(fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s ("+
			"`cashier` varchar(42) NOT NULL,"+
			"`token` varchar(42) NOT NULL,"+
			"`tidx` bigint(20) NOT NULL,"+
			"`sender` varchar(42) NOT NULL,"+
			"`recipient` varchar(42) NOT NULL,"+
			"`amount` varchar(78) NOT NULL,"+
			"`creationTime` timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP,"+
			"`updateTime` timestamp NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,"+
			"`status` varchar(10) NOT NULL DEFAULT '%s',"+
			"`id` varchar(132),"+
			"`blockHeight` bigint(20) NOT NULL,"+
			"`txHash` varchar(66) NOT NULL,"+
			"PRIMARY KEY (`cashier`,`token`,`tidx`),"+
			"KEY `id_index` (`id`),"+
			"KEY `cashier_index` (`cashier`),"+
			"KEY `token_index` (`token`),"+
			"KEY `sender_index` (`sender`),"+
			"KEY `recipient_index` (`recipient`),"+
			"KEY `status_index` (`status`),"+
			"KEY `txHash_index` (`txHash`),"+
			"KEY `blockHeight_index` (`blockHeight`)"+
			") ENGINE=InnoDB DEFAULT CHARSET=latin1;",
		recorder.transferTableName,
		TransferNew,
	)); err != nil {
		return errors.Wrapf(err, "failed to create table %s", recorder.transferTableName)
	}

	return nil
}

// Stop stops the recorder
func (recorder *Recorder) Stop(ctx context.Context) error {
	return recorder.store.Stop(ctx)
}

// AddTransfer creates a new transfer record
func (recorder *Recorder) AddTransfer(tx *Transfer) error {
	if err := recorder.validateID(tx.index); err != nil {
		return err
	}
	if tx.amount.Sign() != 1 {
		return errors.New("amount should be larger than 0")
	}
	result, err := recorder.store.DB().Exec(
		fmt.Sprintf("INSERT IGNORE INTO %s (`cashier`, `token`, `tidx`, `sender`, `recipient`, `amount`, `blockHeight`, `txHash`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)", recorder.transferTableName),
		tx.cashier.Hex(),
		tx.token.Hex(),
		tx.index,
		tx.sender.Hex(),
		tx.recipient.Hex(),
		tx.amount.String(),
		tx.blockHeight,
		tx.txHash.Hex(),
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		log.Printf("duplicate transfer (%s, %s, %d) ignored\n", tx.cashier.Hex(), tx.token.Hex(), tx.index)
	}
	return nil
}

// SettleTransfer marks a record as submitted
func (recorder *Recorder) SettleTransfer(tx *Transfer) error {
	log.Printf("mark transfer %s as settled", tx.id.Hex())
	result, err := recorder.store.DB().Exec(
		fmt.Sprintf("UPDATE %s SET `status`=? WHERE `cashier`=? AND `token`=? AND `tidx`=? AND `status`=?", recorder.transferTableName),
		TransferSettled,
		tx.cashier.Hex(),
		tx.token.Hex(),
		tx.index,
		SubmissionConfirmed,
	)
	if err != nil {
		return err
	}

	return recorder.validateResult(result)
}

// ConfirmTransfer marks a record as settled
func (recorder *Recorder) ConfirmTransfer(tx *Transfer) error {
	log.Printf("mark transfer %s as confirmed", tx.id.Hex())
	result, err := recorder.store.DB().Exec(
		fmt.Sprintf("UPDATE %s SET `status`=?, `id`=? WHERE `cashier`=? AND `token`=? AND `tidx`=? AND `status`=?", recorder.transferTableName),
		SubmissionConfirmed,
		tx.id.Hex(),
		tx.cashier.Hex(),
		tx.token.Hex(),
		tx.index,
		TransferNew,
	)
	if err != nil {
		return err
	}

	return recorder.validateResult(result)
}

// TransfersToSettle returns the list of transfers to confirm
func (recorder *Recorder) TransfersToSettle() ([]*Transfer, error) {
	return recorder.transfers(SubmissionConfirmed)

}

// TransfersToSubmit returns the list of transfers to submit
func (recorder *Recorder) TransfersToSubmit() ([]*Transfer, error) {
	return recorder.transfers(TransferNew)
}

func (recorder *Recorder) transfers(status TransferStatus) ([]*Transfer, error) {
	rows, err := recorder.store.DB().Query(
		fmt.Sprintf(
			"SELECT cashier, token, tidx, sender, recipient, amount, status, id "+
				"FROM %s "+
				"WHERE status=? "+
				"ORDER BY creationTime",
			recorder.transferTableName,
		),
		status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rec []*Transfer
	for rows.Next() {
		tx := &Transfer{}
		var cashier string
		var token string
		var sender string
		var recipient string
		var rawAmount string
		var id sql.NullString
		if err := rows.Scan(&cashier, &token, &tx.index, &sender, &recipient, &rawAmount, &tx.status, &id); err != nil {
			return nil, err
		}
		tx.cashier = common.HexToAddress(cashier)
		tx.token = common.HexToAddress(token)
		tx.sender = common.HexToAddress(sender)
		tx.recipient = common.HexToAddress(recipient)
		if id.Valid {
			tx.id = common.HexToHash(id.String)
		}
		var ok bool
		tx.amount, ok = new(big.Int).SetString(rawAmount, 10)
		if !ok || tx.amount.Sign() != 1 {
			return nil, errors.Errorf("invalid amount %s", rawAmount)
		}
		if toToken, ok := recorder.tokenPairs[tx.token]; ok {
			tx.coToken = toToken
		} else {
			// skip if token is not in whitelist
			continue
		}
		rec = append(rec, tx)
	}
	return rec, nil
}

// TipHeight returns the tip height of all the transfers in the recorder
func (recorder *Recorder) TipHeight() (uint64, error) {
	row := recorder.store.DB().QueryRow(
		fmt.Sprintf("SELECT MAX(blockHeight) FROM %s", recorder.transferTableName),
	)
	var height sql.NullInt64
	if err := row.Scan(&height); err != nil {
		return 0, nil
	}
	if height.Valid {
		return uint64(height.Int64), nil
	}
	return 0, nil
}

func (recorder *Recorder) validateResult(res sql.Result) error {
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.Errorf("The number of rows %d updated is not as expected", affected)
	}
	return nil
}

func (recorder *Recorder) validateID(id uint64) error {
	if id == math.MaxInt64-1 {
		overflow := errors.New("Hit the largest value designed for id, software upgrade needed")
		log.Println(overflow)
		panic(overflow)
	}
	return nil
}
