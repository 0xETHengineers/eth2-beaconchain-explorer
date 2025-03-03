package db

import (
	"context"
	"database/sql"
	"eth2-exporter/cache"
	"eth2-exporter/metrics"
	"eth2-exporter/price"
	"eth2-exporter/types"
	"eth2-exporter/utils"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/lib/pq"
	"github.com/shopspring/decimal"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

func WriteValidatorStatisticsForDay(day uint64) error {
	exportStart := time.Now()
	defer func() {
		metrics.TaskDuration.WithLabelValues("db_update_validator_stats").Observe(time.Since(exportStart).Seconds())
	}()

	firstEpoch, lastEpoch := utils.GetFirstAndLastEpochForDay(day)

	logger.Infof("exporting statistics for day %v (epoch %v to %v)", day, firstEpoch, lastEpoch)

	if err := checkIfDayIsFinalized(day); err != nil {
		return err
	}

	logger.Infof("getting exported state for day %v", day)
	start := time.Now()

	type Exported struct {
		Status              bool `db:"status"`
		FailedAttestations  bool `db:"failed_attestations_exported"`
		SyncDuties          bool `db:"sync_duties_exported"`
		WithdrawalsDeposits bool `db:"withdrawals_deposits_exported"`
		Balance             bool `db:"balance_exported"`
		ClRewards           bool `db:"cl_rewards_exported"`
		ElRewards           bool `db:"el_rewards_exported"`
		TotalPerformance    bool `db:"total_performance_exported"`
		BlockStats          bool `db:"block_stats_exported"`
	}
	exported := Exported{}

	err := ReaderDb.Get(&exported, `
		SELECT 
			status,
			failed_attestations_exported,
			sync_duties_exported,
			withdrawals_deposits_exported,
			balance_exported,
			cl_rewards_exported,
			el_rewards_exported,
			total_performance_exported,
			block_stats_exported
		FROM validator_stats_status 
		WHERE day = $1;
		`, day)

	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("error retrieving exported state: %v", err)
	}
	logger.Infof("getting exported state took %v", time.Since(start))

	if exported.FailedAttestations && exported.SyncDuties && exported.WithdrawalsDeposits && exported.Balance && exported.ClRewards && exported.ElRewards && exported.TotalPerformance && exported.BlockStats && exported.Status {
		logger.Infof("Skipping day %v as it is already exported", day)
		return nil
	}

	if exported.FailedAttestations {
		logger.Infof("Skipping failed attestations")
	} else if err := WriteValidatorFailedAttestationsStatisticsForDay(day); err != nil {
		return err
	}

	if exported.SyncDuties {
		logger.Infof("Skipping sync duties")
	} else if err := WriteValidatorSyncDutiesForDay(day); err != nil {
		return err
	}

	if exported.WithdrawalsDeposits {
		logger.Infof("Skipping withdrawals / deposits")
	} else if err := WriteValidatorDepositWithdrawals(day); err != nil {
		return err
	}

	if exported.BlockStats {
		logger.Infof("Skipping block stats")
	} else if err := WriteValidatorBlockStats(day); err != nil {
		return err
	}

	if exported.Balance {
		logger.Infof("Skipping balances")
	} else if err := WriteValidatorBalances(day); err != nil {
		return err
	}

	if exported.ClRewards {
		logger.Infof("Skipping cl rewards")
	} else if err := WriteValidatorClIcome(day); err != nil {
		return err
	}

	if exported.ElRewards {
		logger.Infof("Skipping el rewards")
	} else if err := WriteValidatorElIcome(day); err != nil {
		return err
	}

	if exported.TotalPerformance {
		logger.Infof("Skipping total performance")
	} else if err := WriteValidatorTotalPerformance(day); err != nil {
		return err
	}

	if err := WriteValidatorStatsExported(day); err != nil {
		return err
	}

	logger.Infof("statistics export of day %v completed, took %v", day, time.Since(exportStart))
	return nil
}

func WriteValidatorStatsExported(day uint64) error {
	tx, err := WriterDb.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	start := time.Now()

	logger.Infof("marking day export as completed in the status table")
	_, err = tx.Exec(`
		UPDATE validator_stats_status
		SET status = true
		WHERE day=$1
		AND failed_attestations_exported = true
		AND sync_duties_exported = true
		AND withdrawals_deposits_exported = true
		AND balance_exported = true
		AND cl_rewards_exported = true
		AND el_rewards_exported = true
		AND total_performance_exported = true
		AND block_stats_exported = true;
		`, day)
	if err != nil {
		return err
	}
	logger.Infof("marking completed, took %v", time.Since(start))

	err = tx.Commit()
	if err != nil {
		return err
	}
	return nil
}

func WriteValidatorTotalPerformance(day uint64) error {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute*10))
	defer cancel()
	exportStart := time.Now()
	defer func() {
		metrics.TaskDuration.WithLabelValues("db_update_validator_total_performance_stats").Observe(time.Since(exportStart).Seconds())
	}()

	if err := checkIfDayIsFinalized(day); err != nil {
		return err
	}

	start := time.Now()
	logger.Infof("validating if required data has been exported for total performance")
	type Exported struct {
		LastClRewards    bool `db:"last_cl_rewards_exported"`
		LastElRewards    bool `db:"last_el_rewards_exported"`
		CurrentCLRewards bool `db:"cur_cl_rewards_exported"`
		CurrentElRewards bool `db:"cur_el_rewards_exported"`
	}
	exported := Exported{}
	err := ReaderDb.Get(&exported, `
		SELECT 
			last.cl_rewards_exported as last_cl_rewards_exported, 
			last.el_rewards_exported as last_el_rewards_exported, 
			cur.cl_rewards_exported as cur_cl_rewards_exported, 
			cur.el_rewards_exported as cur_el_rewards_exported
		FROM validator_stats_status cur
		INNER JOIN validator_stats_status last 
				ON last.day = GREATEST(cur.day - 1, 0)
		WHERE cur.day = $1;
	`, day)

	if err != nil {
		return fmt.Errorf("error retrieving required data: %v", err)
	} else if !exported.CurrentCLRewards || !exported.CurrentElRewards || !exported.LastClRewards || !exported.LastElRewards {
		return fmt.Errorf("missing required export: cur cl rewards: %v, cur el rewards: %v, last cl rewards: %v, last el rewards: %v", !exported.CurrentCLRewards, !exported.CurrentElRewards, !exported.LastClRewards, !exported.LastElRewards)
	}
	logger.Infof("validating completed, took %v", time.Since(start))

	start = time.Now()

	logger.Infof("exporting total income stats")
	maxValidatorIndex, err := GetTotalValidatorsCount()
	if err != nil {
		return err
	}
	g, gCtx := errgroup.WithContext(ctx)
	batchSize := 1000
	for b := 0; b <= int(maxValidatorIndex); b += batchSize {
		start := b
		end := b + batchSize
		if int(maxValidatorIndex) < end {
			end = int(maxValidatorIndex)
		}
		g.Go(func() error {
			select {
			case <-gCtx.Done():
				return nil
			default:
			}
			_, err = WriterDb.Exec(`
				INSERT INTO validator_stats (validatorindex, day, cl_rewards_gwei_total, cl_proposer_rewards_gwei_total, el_rewards_wei_total, mev_rewards_wei_total) (
					SELECT 
						vs1.validatorindex, 
						vs1.day, 
						COALESCE(vs1.cl_rewards_gwei, 0) + COALESCE(vs2.cl_rewards_gwei_total, 0) AS cl_rewards_gwei_total_new, 
						COALESCE(vs1.cl_proposer_rewards_gwei, 0) + COALESCE(vs2.cl_proposer_rewards_gwei_total, 0) AS cl_proposer_rewards_gwei_total_new, 
						COALESCE(vs1.el_rewards_wei, 0) + COALESCE(vs2.el_rewards_wei_total, 0) AS el_rewards_wei_total_new, 
						COALESCE(vs1.mev_rewards_wei, 0) + COALESCE(vs2.mev_rewards_wei_total, 0) AS mev_rewards_wei_total_new 
					FROM validator_stats vs1 LEFT JOIN validator_stats vs2 ON vs2.day = vs1.day - 1 AND vs2.validatorindex = vs1.validatorindex WHERE vs1.day = $1 AND vs1.validatorindex >= $2 AND vs1.validatorindex < $3
				) ON CONFLICT (validatorindex, day) DO UPDATE SET 
					cl_rewards_gwei_total = excluded.cl_rewards_gwei_total,
					cl_proposer_rewards_gwei_total = excluded.cl_proposer_rewards_gwei_total,
					el_rewards_wei_total = excluded.el_rewards_wei_total,
					mev_rewards_wei_total = excluded.mev_rewards_wei_total;
				`, day, start, end)
			if err != nil {
				return err
			}

			_, err = WriterDb.Exec(`insert into validator_performance (
				validatorindex,
				balance,
				performance1d,
				performance7d,
				performance31d,
				performance365d,

				rank7d,

				cl_performance_1d,
				cl_performance_7d,
				cl_performance_31d,
				cl_performance_365d,
				cl_performance_total,
				cl_proposer_performance_total,

				el_performance_1d,
				el_performance_7d,
				el_performance_31d,
				el_performance_365d,
				el_performance_total,

				mev_performance_1d,
				mev_performance_7d,
				mev_performance_31d,
				mev_performance_365d,
				mev_performance_total
				) (
					select 
					vs_now.validatorindex, 
						COALESCE(vs_now.end_balance, 0) as balance, 
						0 as performance1d, 
						0 as performance7d, 
						0 as performance31d, 
						0 as performance365d, 
						0 as rank7d,

						coalesce(vs_now.cl_rewards_gwei_total, 0) - coalesce(vs_1d.cl_rewards_gwei_total, 0) as cl_performance_1d, 
						coalesce(vs_now.cl_rewards_gwei_total, 0) - coalesce(vs_7d.cl_rewards_gwei_total, 0) as cl_performance_7d, 
						coalesce(vs_now.cl_rewards_gwei_total, 0) - coalesce(vs_31d.cl_rewards_gwei_total, 0) as cl_performance_31d, 
						coalesce(vs_now.cl_rewards_gwei_total, 0) - coalesce(vs_365d.cl_rewards_gwei_total, 0) as cl_performance_365d,
						coalesce(vs_now.cl_rewards_gwei_total, 0) as cl_performance_total, 
						coalesce(vs_now.cl_proposer_rewards_gwei_total, 0) as cl_proposer_performance_total, 
						
						coalesce(vs_now.el_rewards_wei_total, 0) - coalesce(vs_1d.el_rewards_wei_total, 0) as el_performance_1d, 
						coalesce(vs_now.el_rewards_wei_total, 0) - coalesce(vs_7d.el_rewards_wei_total, 0) as el_performance_7d, 
						coalesce(vs_now.el_rewards_wei_total, 0) - coalesce(vs_31d.el_rewards_wei_total, 0) as el_performance_31d, 
						coalesce(vs_now.el_rewards_wei_total, 0) - coalesce(vs_365d.el_rewards_wei_total, 0) as el_performance_365d,
						coalesce(vs_now.el_rewards_wei_total, 0) as el_performance_total, 
						
						coalesce(vs_now.mev_rewards_wei_total, 0) - coalesce(vs_1d.mev_rewards_wei_total, 0) as mev_performance_1d, 
						coalesce(vs_now.mev_rewards_wei_total, 0) - coalesce(vs_7d.mev_rewards_wei_total, 0) as mev_performance_7d, 
						coalesce(vs_now.mev_rewards_wei_total, 0) - coalesce(vs_31d.mev_rewards_wei_total, 0) as mev_performance_31d, 
						coalesce(vs_now.mev_rewards_wei_total, 0) - coalesce(vs_365d.mev_rewards_wei_total, 0) as mev_performance_365d,
						coalesce(vs_now.mev_rewards_wei_total, 0) as mev_performance_total
					from validator_stats vs_now
					left join validator_stats vs_1d on vs_1d.validatorindex = vs_now.validatorindex and vs_1d.day = $2
					left join validator_stats vs_7d on vs_7d.validatorindex = vs_now.validatorindex and vs_7d.day = $3
					left join validator_stats vs_31d on vs_31d.validatorindex = vs_now.validatorindex and vs_31d.day = $4
					left join validator_stats vs_365d on vs_365d.validatorindex = vs_now.validatorindex and vs_365d.day = $5
					where vs_now.day = $1 AND vs_now.validatorindex >= $6 AND vs_now.validatorindex < $7
				) 
				on conflict (validatorindex) do update set 
					balance = excluded.balance, 
					performance1d=excluded.performance1d,
					performance7d=excluded.performance7d,
					performance31d=excluded.performance31d,
					performance365d=excluded.performance365d,

					rank7d=excluded.rank7d,

					cl_performance_1d=excluded.cl_performance_1d,
					cl_performance_7d=excluded.cl_performance_7d,
					cl_performance_31d=excluded.cl_performance_31d,
					cl_performance_365d=excluded.cl_performance_365d,
					cl_performance_total=excluded.cl_performance_total,
					cl_proposer_performance_total=excluded.cl_proposer_performance_total,

					el_performance_1d=excluded.el_performance_1d,
					el_performance_7d=excluded.el_performance_7d,
					el_performance_31d=excluded.el_performance_31d,
					el_performance_365d=excluded.el_performance_365d,
					el_performance_total=excluded.el_performance_total,

					mev_performance_1d=excluded.mev_performance_1d,
					mev_performance_7d=excluded.mev_performance_7d,
					mev_performance_31d=excluded.mev_performance_31d,
					mev_performance_365d=excluded.mev_performance_365d,
					mev_performance_total=excluded.mev_performance_total
			;`, day, int64(day)-1, int64(day)-7, int64(day)-31, int64(day)-365, start, end)

			logger.Infof("populate validator_performance table done for batch %v", start)
			return err
		})
	}
	if err = g.Wait(); err != nil {
		logrus.Error(err)
		return err
	}
	logger.Infof("export completed, took %v", time.Since(start))
	start = time.Now()
	logger.Infof("populate validator_performance rank7d")

	_, err = WriterDb.Exec(`
		insert into validator_performance (                                                                                                 
			validatorindex,          
			balance,             
			performance1d,
			performance7d,
			performance31d,  
			performance365d,                                                                                             
			rank7d
		) (
			select validatorindex, 0, 0, 0, 0, 0, row_number() over(order by validator_performance.cl_performance_7d desc) as rank7d from validator_performance
		) 
			on conflict (validatorindex) do update set 
				rank7d=excluded.rank7d
		;
		`)
	if err != nil {
		return err
	}

	logger.Infof("export completed, took %v", time.Since(start))

	if err = markColumnExported(day, "total_performance_exported"); err != nil {
		return err
	}

	logger.Infof("total performance statistics export of day %v completed, took %v", day, time.Since(exportStart))
	return nil
}

func WriteValidatorBlockStats(day uint64) error {
	exportStart := time.Now()
	defer func() {
		metrics.TaskDuration.WithLabelValues("db_update_validator_block_stats").Observe(time.Since(exportStart).Seconds())
	}()

	if err := checkIfDayIsFinalized(day); err != nil {
		return err
	}

	firstEpoch, lastEpoch := utils.GetFirstAndLastEpochForDay(day)

	tx, err := WriterDb.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	start := time.Now()

	logger.Infof("exporting proposed_blocks, missed_blocks and orphaned_blocks statistics")
	_, err = tx.Exec(`
		insert into validator_stats (validatorindex, day, proposed_blocks, missed_blocks, orphaned_blocks) 
		(
			select proposer, $3, sum(case when status = '1' then 1 else 0 end), sum(case when status = '2' then 1 else 0 end), sum(case when status = '3' then 1 else 0 end)
			from blocks
			where epoch >= $1 and epoch <= $2
			group by proposer
		) 
		on conflict (validatorindex, day) do update set proposed_blocks = excluded.proposed_blocks, missed_blocks = excluded.missed_blocks, orphaned_blocks = excluded.orphaned_blocks;`,
		firstEpoch, lastEpoch, day)
	if err != nil {
		return err
	}
	logger.Infof("export completed, took %v", time.Since(start))

	start = time.Now()
	logger.Infof("exporting attester_slashings and proposer_slashings statistics")
	_, err = tx.Exec(`
		insert into validator_stats (validatorindex, day, attester_slashings, proposer_slashings) 
		(
			select proposer, $3, sum(attesterslashingscount), sum(proposerslashingscount)
			from blocks
			where epoch >= $1 and epoch <= $2 and status = '1'
			group by proposer
		) 
		on conflict (validatorindex, day) do update set attester_slashings = excluded.attester_slashings, proposer_slashings = excluded.proposer_slashings;`,
		firstEpoch, lastEpoch, day)
	if err != nil {
		return err
	}

	if err = tx.Commit(); err != nil {
		return err
	}
	logger.Infof("export completed, took %v", time.Since(start))

	if err = markColumnExported(day, "block_stats_exported"); err != nil {
		return err
	}

	logger.Infof("block statistics export of day %v completed, took %v", day, time.Since(exportStart))
	return nil
}

func WriteValidatorElIcome(day uint64) error {
	exportStart := time.Now()
	defer func() {
		metrics.TaskDuration.WithLabelValues("db_update_validator_el_income_stats").Observe(time.Since(exportStart).Seconds())
	}()

	if err := checkIfDayIsFinalized(day); err != nil {
		return err
	}

	firstEpoch, lastEpoch := utils.GetFirstAndLastEpochForDay(day)

	tx, err := WriterDb.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	start := time.Now()

	logger.Infof("exporting mev & el rewards")

	type Container struct {
		Slot            uint64 `db:"slot"`
		ExecBlockNumber uint64 `db:"exec_block_number"`
		Proposer        uint64 `db:"proposer"`
		TxFeeReward     *big.Int
		MevReward       *big.Int
	}

	blocks := make([]*Container, 0)
	blocksMap := make(map[uint64]*Container)

	err = tx.Select(&blocks, "SELECT slot, exec_block_number, proposer FROM blocks WHERE epoch >= $1 AND epoch <= $2 AND exec_block_number > 0 AND status = '1'", firstEpoch, lastEpoch)
	if err != nil {
		return fmt.Errorf("error retrieving blocks data: %v", err)
	}

	numbers := make([]uint64, 0, len(blocks))

	for _, b := range blocks {
		numbers = append(numbers, b.ExecBlockNumber)
		blocksMap[b.ExecBlockNumber] = b
	}

	blocksData, err := BigtableClient.GetBlocksIndexedMultiple(numbers, uint64(len(numbers)))
	if err != nil {
		return fmt.Errorf("error in GetBlocksIndexedMultiple: %v", err)
	}

	relaysData, err := GetRelayDataForIndexedBlocks(blocksData)
	if err != nil {
		return fmt.Errorf("error in GetRelayDataForIndexedBlocks: %v", err)
	}

	proposerRewards := make(map[uint64]*Container)
	for _, b := range blocksData {
		proposer := blocksMap[b.Number].Proposer

		if proposerRewards[proposer] == nil {
			proposerRewards[proposer] = &Container{
				MevReward:   big.NewInt(0),
				TxFeeReward: big.NewInt(0),
			}
		}

		txFeeReward := new(big.Int).SetBytes(b.TxReward)
		proposerRewards[proposer].TxFeeReward = new(big.Int).Add(txFeeReward, proposerRewards[proposer].TxFeeReward)

		mevReward, ok := relaysData[common.BytesToHash(b.Hash)]

		if ok {
			proposerRewards[proposer].MevReward = new(big.Int).Add(mevReward.MevBribe.BigInt(), proposerRewards[proposer].MevReward)
		} else {
			proposerRewards[proposer].MevReward = new(big.Int).Add(txFeeReward, proposerRewards[proposer].MevReward)
		}
	}
	logrus.Infof("retrieved mev / el rewards data for %v proposer", len(proposerRewards))

	if len(proposerRewards) > 0 {
		numArgs := 4
		valueStrings := make([]string, 0, len(proposerRewards))
		valueArgs := make([]interface{}, 0, len(proposerRewards)*numArgs)
		i := 0
		for proposer, rewards := range proposerRewards {

			valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d, $%d)", i*numArgs+1, i*numArgs+2, i*numArgs+3, i*numArgs+4))
			valueArgs = append(valueArgs, proposer)
			valueArgs = append(valueArgs, day)
			valueArgs = append(valueArgs, rewards.TxFeeReward.String())
			valueArgs = append(valueArgs, rewards.MevReward.String())

			i++
		}
		stmt := fmt.Sprintf(`
				INSERT INTO validator_stats (validatorindex, day, el_rewards_wei, mev_rewards_wei) VALUES
				%s
				ON CONFLICT(validatorindex, day) DO UPDATE SET el_rewards_wei = excluded.el_rewards_wei, mev_rewards_wei = excluded.mev_rewards_wei;`,
			strings.Join(valueStrings, ","))
		_, err = tx.Exec(stmt, valueArgs...)
		if err != nil {
			return err
		}
	}

	if err = tx.Commit(); err != nil {
		return err
	}
	logger.Infof("export completed, took %v", time.Since(start))

	if err = markColumnExported(day, "el_rewards_exported"); err != nil {
		return err
	}

	logger.Infof("el rewards statistics export of day %v completed, took %v", day, time.Since(exportStart))
	return nil
}

func WriteValidatorClIcome(day uint64) error {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute*10))
	defer cancel()
	exportStart := time.Now()
	defer func() {
		metrics.TaskDuration.WithLabelValues("db_update_validator_cl_income_stats").Observe(time.Since(exportStart).Seconds())
	}()

	if err := checkIfDayIsFinalized(day); err != nil {
		return err
	}

	start := time.Now()
	logger.Infof("validating if required data has been exported for cl rewards")
	type Exported struct {
		LastBalanceExported                bool `db:"last_balance_exported"`
		CurrentBalanceExported             bool `db:"cur_balance_exported"`
		CurrentWithdrawalsDepositsExported bool `db:"cur_withdrawals_deposits_exported"`
	}
	exported := Exported{}
	err := ReaderDb.Get(&exported, `
		SELECT last.balance_exported as last_balance_exported, cur.balance_exported as cur_balance_exported, cur.withdrawals_deposits_exported as cur_withdrawals_deposits_exported
		FROM validator_stats_status cur
		INNER JOIN validator_stats_status last 
				ON last.day = GREATEST(cur.day - 1, 0)
		WHERE cur.day = $1;
	`, day)

	if err != nil {
		return fmt.Errorf("error retrieving required data: %v", err)
	} else if !exported.CurrentBalanceExported || !exported.CurrentWithdrawalsDepositsExported || !exported.LastBalanceExported {
		return fmt.Errorf("missing required export: cur balance: %v, cur withdrwals/deposits: %v, last balance: %v", !exported.CurrentBalanceExported, !exported.CurrentWithdrawalsDepositsExported, !exported.LastBalanceExported)
	}
	logger.Infof("validating took %v", time.Since(start))

	start = time.Now()
	firstEpoch, lastEpoch := utils.GetFirstAndLastEpochForDay(day)

	logger.Infof("exporting cl_rewards_wei statistics")
	incomeStats, err := BigtableClient.GetAggregatedValidatorIncomeDetailsHistory([]uint64{}, firstEpoch, lastEpoch)
	if err != nil {
		return err
	}
	logrus.Infof("getting cl income done in %v, now we export them to the db", time.Since(start))
	start = time.Now()

	maxValidatorIndex := uint64(0)

	for validator := range incomeStats {
		if validator > maxValidatorIndex {
			maxValidatorIndex = validator
		}
	}
	maxValidatorIndex++

	g, gCtx := errgroup.WithContext(ctx)

	numArgs := 3
	batchSize := 100 // max parameters: 65535 / 3, but it's faster in smaller batches
	for b := 0; b <= int(maxValidatorIndex); b += batchSize {
		start := b
		end := b + batchSize
		if int(maxValidatorIndex) < end {
			end = int(maxValidatorIndex)
		}

		logrus.Info(start, end)
		valueStrings := make([]string, 0, batchSize)
		valueArgs := make([]interface{}, 0, batchSize*numArgs)
		for i := start; i < end; i++ {
			clProposerRewards := uint64(0)

			if incomeStats[uint64(i)] != nil {
				clProposerRewards = incomeStats[uint64(i)].ProposerAttestationInclusionReward + incomeStats[uint64(i)].ProposerSlashingInclusionReward + incomeStats[uint64(i)].ProposerSyncInclusionReward
			}
			valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d)", (i-start)*numArgs+1, (i-start)*numArgs+2, (i-start)*numArgs+3))
			valueArgs = append(valueArgs, i)
			valueArgs = append(valueArgs, day)
			valueArgs = append(valueArgs, clProposerRewards)
		}
		stmt := fmt.Sprintf(`
		insert into validator_stats (validatorindex, day, cl_proposer_rewards_gwei) VALUES
		%s
		on conflict (validatorindex, day) do update set cl_proposer_rewards_gwei = excluded.cl_proposer_rewards_gwei;`,
			strings.Join(valueStrings, ","))

		g.Go(func() error {
			select {
			case <-gCtx.Done():
				return nil
			default:
			}
			_, err := WriterDb.Exec(stmt, valueArgs...)
			if err != nil {
				return err
			}
			logrus.Infof("saving validator proposer rewards gwei batch %v completed", start)
			stmt = `
				INSERT INTO validator_stats (validatorindex, day, cl_rewards_gwei) 
				(
					SELECT cur.validatorindex, cur.day, COALESCE(cur.end_balance, 0) - COALESCE(last.end_balance, 0) + COALESCE(cur.withdrawals_amount, 0) - COALESCE(cur.deposits_amount, 0) AS cl_rewards_gwei
					FROM validator_stats cur
					INNER JOIN validator_stats last 
						ON cur.validatorindex = last.validatorindex AND last.day = GREATEST(cur.day - 1, 0)
					WHERE cur.day = $1 AND cur.validatorindex >= $2 AND cur.validatorindex < $3
				)
				ON CONFLICT (validatorindex, day) DO
					UPDATE SET cl_rewards_gwei = excluded.cl_rewards_gwei;`
			if day == 0 {
				stmt = `
					INSERT INTO validator_stats (validatorindex, day, cl_rewards_gwei) 
					(
						SELECT cur.validatorindex, cur.day, COALESCE(cur.end_balance, 0) - COALESCE(cur.start_balance,0) + COALESCE(cur.withdrawals_amount, 0) - COALESCE(cur.deposits_amount, 0) AS cl_rewards_gwei
						FROM validator_stats cur
						WHERE cur.day = $1 AND cur.validatorindex >= $2 AND cur.validatorindex < $3
					)
					ON CONFLICT (validatorindex, day) DO
						UPDATE SET cl_rewards_gwei = excluded.cl_rewards_gwei;`
			}
			_, err = WriterDb.Exec(stmt, day, start, end)
			if err != nil {
				return err
			}
			logrus.Infof("saving validator cl rewards gwei batch %v completed", start)
			return nil
		})
	}

	if err = g.Wait(); err != nil {
		logrus.Error(err)
		return err
	}

	logger.Infof("export completed, took %v", time.Since(start))

	if err = markColumnExported(day, "cl_rewards_exported"); err != nil {
		return err
	}

	logger.Infof("cl rewards statistics export of day %v completed, took %v", day, time.Since(exportStart))
	return nil
}

func WriteValidatorBalances(day uint64) error {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute*10))
	defer cancel()

	exportStart := time.Now()
	defer func() {
		metrics.TaskDuration.WithLabelValues("db_update_validator_balances_stats").Observe(time.Since(exportStart).Seconds())
	}()

	if err := checkIfDayIsFinalized(day); err != nil {
		return err
	}

	firstEpoch, lastEpoch := utils.GetFirstAndLastEpochForDay(day)

	start := time.Now()

	logger.Infof("exporting min_balance, max_balance, min_effective_balance, max_effective_balance, start_balance, start_effective_balance, end_balance and end_effective_balance statistics")
	balanceStatistics, err := BigtableClient.GetValidatorBalanceStatistics(firstEpoch, lastEpoch)
	if err != nil {
		return err
	}

	balanceStatsArr := make([]*types.ValidatorBalanceStatistic, 0, len(balanceStatistics))
	for _, stat := range balanceStatistics {
		balanceStatsArr = append(balanceStatsArr, stat)
	}
	logger.Infof("fetching balance completed, took %v, now we save it", time.Since(start))
	start = time.Now()

	g, gCtx := errgroup.WithContext(ctx)

	batchSize := 100 // max parameters: 65535 / 10, but we are faster with smaller batch sizes
	for b := 0; b < len(balanceStatsArr); b += batchSize {
		start := b
		end := b + batchSize
		if len(balanceStatsArr) < end {
			end = len(balanceStatsArr)
		}

		numArgs := 10
		valueStrings := make([]string, 0, batchSize)
		valueArgs := make([]interface{}, 0, batchSize*numArgs)

		g.Go(func() error {
			select {
			case <-gCtx.Done():
				return nil
			default:
			}
			defer logger.Infof("saving validator balance batch %v completed", start)
			for i, stat := range balanceStatsArr[start:end] {
				valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)", i*numArgs+1, i*numArgs+2, i*numArgs+3, i*numArgs+4, i*numArgs+5, i*numArgs+6, i*numArgs+7, i*numArgs+8, i*numArgs+9, i*numArgs+10))
				valueArgs = append(valueArgs, stat.Index)
				valueArgs = append(valueArgs, day)
				valueArgs = append(valueArgs, stat.MinBalance)
				valueArgs = append(valueArgs, stat.MaxBalance)
				valueArgs = append(valueArgs, stat.MinEffectiveBalance)
				valueArgs = append(valueArgs, stat.MaxEffectiveBalance)
				valueArgs = append(valueArgs, stat.StartBalance)
				valueArgs = append(valueArgs, stat.StartEffectiveBalance)
				valueArgs = append(valueArgs, stat.EndBalance)
				valueArgs = append(valueArgs, stat.EndEffectiveBalance)
			}
			stmt := fmt.Sprintf(`
				insert into validator_stats (validatorindex, day, min_balance, max_balance, min_effective_balance, max_effective_balance, start_balance, start_effective_balance, end_balance, end_effective_balance) VALUES
				%s
				on conflict (validatorindex, day) do update set min_balance = excluded.min_balance, max_balance = excluded.max_balance, min_effective_balance = excluded.min_effective_balance, max_effective_balance = excluded.max_effective_balance, start_balance = excluded.start_balance, start_effective_balance = excluded.start_effective_balance, end_balance = excluded.end_balance, end_effective_balance = excluded.end_effective_balance;`,
				strings.Join(valueStrings, ","))
			_, err := WriterDb.Exec(stmt, valueArgs...)

			return err
		})
	}

	if err = g.Wait(); err != nil {
		logrus.Error(err)
		return err
	}

	logger.Infof("export completed, took %v", time.Since(start))

	if err = markColumnExported(day, "balance_exported"); err != nil {
		return err
	}

	logger.Infof("balance statistics export of day %v completed, took %v", day, time.Since(exportStart))
	return nil
}

func WriteValidatorDepositWithdrawals(day uint64) error {
	exportStart := time.Now()
	defer func() {
		metrics.TaskDuration.WithLabelValues("db_update_validator_deposit_withdrawal_stats").Observe(time.Since(exportStart).Seconds())
	}()

	if err := checkIfDayIsFinalized(day); err != nil {
		return err
	}

	firstEpoch, lastEpoch := utils.GetFirstAndLastEpochForDay(day)
	// for getting the withrawals / deposits for the current day we have to go 1 epoch in the past as they affect the balance one epoch after they have happend
	if firstEpoch > 0 {
		firstEpoch--
	}
	lastEpoch--

	tx, err := WriterDb.Beginx()
	if err != nil {
		logrus.Errorf("error WriterDb.Beginx %v", err)
		return err
	}
	defer tx.Rollback()

	start := time.Now()
	logrus.Infof("Update Withdrawals + Deposits for day [%v] epoch %v -> %v", day, firstEpoch, lastEpoch)

	logger.Infof("exporting deposits and deposits_amount statistics")
	depositsQry := `
		insert into validator_stats (validatorindex, day, deposits, deposits_amount) 
		(
			select validators.validatorindex, $3, count(*), sum(amount)
			from blocks_deposits
			inner join validators on blocks_deposits.publickey = validators.pubkey
			inner join blocks on blocks_deposits.block_root = blocks.blockroot
			where blocks.epoch >= $1 and blocks.epoch <= $2 and blocks.status = '1' and blocks_deposits.valid_signature
			group by validators.validatorindex
		) 
		on conflict (validatorindex, day) do
			update set deposits = excluded.deposits, 
			deposits_amount = excluded.deposits_amount;`
	if day == 0 {
		// genesis-deposits will be added to block 0 by the exporter which is technically not 100% correct
		// since deposits will be added to the validator-balance only after the block which includes the deposits.
		// to ease the calculation of validator-income (considering deposits) we set the day of genesis-deposits to -1.
		depositsQry = `
			insert into validator_stats (validatorindex, day, deposits, deposits_amount)
			(
				select validators.validatorindex, case when block_slot = 0 then -1 else $3 end as day, count(*), sum(amount)
				from blocks_deposits
				inner join validators on blocks_deposits.publickey = validators.pubkey
				inner join blocks on blocks_deposits.block_root = blocks.blockroot
				where blocks.epoch >= $1 and blocks.epoch <= $2 and blocks.status = '1'
				group by validators.validatorindex, day
			) 
			on conflict (validatorindex, day) do
				update set deposits = excluded.deposits, 
				deposits_amount = excluded.deposits_amount;`
	}

	_, err = tx.Exec(depositsQry, firstEpoch, lastEpoch, day)
	if err != nil {
		return err
	}
	logger.Infof("export completed, took %v", time.Since(start))

	start = time.Now()
	logger.Infof("exporting withdrawals and withdrawals_amount statistics")
	withdrawalsQuery := `
		insert into validator_stats (validatorindex, day, withdrawals, withdrawals_amount) 
		(
			select validatorindex, $3, count(*), sum(amount)
			from blocks_withdrawals
			inner join blocks on blocks_withdrawals.block_root = blocks.blockroot
			where block_slot >= $1 and block_slot < $2 and blocks.status = '1'
			group by validatorindex
		) 
		on conflict (validatorindex, day) do
			update set withdrawals = excluded.withdrawals, 
			withdrawals_amount = excluded.withdrawals_amount;`
	_, err = tx.Exec(withdrawalsQuery, firstEpoch*utils.Config.Chain.Config.SlotsPerEpoch, (lastEpoch+1)*utils.Config.Chain.Config.SlotsPerEpoch, day)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}

	logger.Infof("export completed, took %v", time.Since(start))

	if err = markColumnExported(day, "withdrawals_deposits_exported"); err != nil {
		return err
	}

	logger.Infof("deposits and withdrawals statistics export of day %v completed, took %v", day, time.Since(exportStart))
	return nil
}

func WriteValidatorSyncDutiesForDay(day uint64) error {
	exportStart := time.Now()
	defer func() {
		metrics.TaskDuration.WithLabelValues("db_update_validator_sync_stats").Observe(time.Since(exportStart).Seconds())
	}()

	if err := checkIfDayIsFinalized(day); err != nil {
		return err
	}

	startEpoch, endEpoch := utils.GetFirstAndLastEpochForDay(day)

	start := time.Now()
	logrus.Infof("Update Sync duties for day [%v] epoch %v -> %v", day, startEpoch, endEpoch)

	syncStats, err := BigtableClient.GetValidatorSyncDutiesStatistics([]uint64{}, startEpoch, endEpoch)
	if err != nil {
		return err
	}
	logrus.Infof("getting sync duties done in %v, now we export them to the db", time.Since(start))
	start = time.Now()

	syncStatsArr := make([]*types.ValidatorSyncDutiesStatistic, 0, len(syncStats))
	for _, stat := range syncStats {
		syncStatsArr = append(syncStatsArr, stat)
	}

	tx, err := WriterDb.Beginx()
	if err != nil {
		logrus.Errorf("error WriterDb.Beginx %v", err)
		return err
	}
	defer tx.Rollback()

	batchSize := 13000 // max parameters: 65535
	for b := 0; b < len(syncStatsArr); b += batchSize {
		start := b
		end := b + batchSize
		if len(syncStatsArr) < end {
			end = len(syncStatsArr)
		}

		numArgs := 5
		valueStrings := make([]string, 0, batchSize)
		valueArgs := make([]interface{}, 0, batchSize*numArgs)
		for i, stat := range syncStatsArr[start:end] {
			valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d)", i*numArgs+1, i*numArgs+2, i*numArgs+3, i*numArgs+4, i*numArgs+5))
			valueArgs = append(valueArgs, stat.Index)
			valueArgs = append(valueArgs, day)
			valueArgs = append(valueArgs, stat.ParticipatedSync)
			valueArgs = append(valueArgs, stat.MissedSync)
			valueArgs = append(valueArgs, stat.OrphanedSync)
		}
		stmt := fmt.Sprintf(`
			insert into validator_stats (validatorindex, day, participated_sync, missed_sync, orphaned_sync)  VALUES
			%s
			on conflict (validatorindex, day) do update set participated_sync = excluded.participated_sync, missed_sync = excluded.missed_sync, orphaned_sync = excluded.orphaned_sync;`,
			strings.Join(valueStrings, ","))
		_, err := tx.Exec(stmt, valueArgs...)
		if err != nil {
			return err
		}

		logrus.Infof("saving sync statistics batch %v completed", b)
	}

	if err = tx.Commit(); err != nil {
		return err
	}

	logger.Infof("export completed, took %v", time.Since(start))

	if err = markColumnExported(day, "sync_duties_exported"); err != nil {
		return err
	}

	logger.Infof("sync duties and statistics export of day %v completed, took %v", day, time.Since(exportStart))
	return nil
}

func WriteValidatorFailedAttestationsStatisticsForDay(day uint64) error {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Minute*10))
	defer cancel()
	exportStart := time.Now()
	defer func() {
		metrics.TaskDuration.WithLabelValues("db_update_validator_failed_att_stats").Observe(time.Since(exportStart).Seconds())
	}()

	if err := checkIfDayIsFinalized(day); err != nil {
		return err
	}

	firstEpoch, lastEpoch := utils.GetFirstAndLastEpochForDay(day)

	start := time.Now()

	logrus.Infof("exporting 'failed attestations' statistics firstEpoch: %v lastEpoch: %v", firstEpoch, lastEpoch)

	// first key is the batch start index and the second is the validator id
	failed := map[uint64]map[uint64]*types.ValidatorFailedAttestationsStatistic{}
	mux := sync.Mutex{}
	g, gCtx := errgroup.WithContext(ctx)
	epochBatchSize := uint64(2) // Fetching 2 Epochs per batch seems to be the fastest way to go
	for i := firstEpoch; i < lastEpoch; i += epochBatchSize {
		fromEpoch := i
		toEpoch := fromEpoch + epochBatchSize
		if toEpoch >= lastEpoch {
			toEpoch = lastEpoch
		} else {
			toEpoch--
		}
		g.Go(func() error {
			select {
			case <-gCtx.Done():
				return nil
			default:
			}
			ma, err := BigtableClient.GetValidatorFailedAttestationsCount([]uint64{}, fromEpoch, toEpoch)
			if err != nil {
				logrus.Errorf("error getting 'failed attestations' %v", err)
				return err
			}
			mux.Lock()
			failed[fromEpoch] = ma
			mux.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return err
	}

	validatorMap := map[uint64]*types.ValidatorFailedAttestationsStatistic{}
	for _, f := range failed {

		for key, val := range f {
			if validatorMap[key] == nil {
				validatorMap[key] = val
			} else {
				validatorMap[key].MissedAttestations += val.MissedAttestations
				validatorMap[key].OrphanedAttestations += val.OrphanedAttestations
			}
		}
	}

	logrus.Infof("fetching 'failed attestations' done in %v, now we export them to the db", time.Since(start))
	start = time.Now()
	maArr := make([]*types.ValidatorFailedAttestationsStatistic, 0, len(validatorMap))

	for _, stat := range validatorMap {
		maArr = append(maArr, stat)
	}

	g, gCtx = errgroup.WithContext(ctx)

	batchSize := 100 // max: 65535 / 4, but we are faster with smaller batches
	for b := 0; b < len(maArr); b += batchSize {

		start := b
		end := b + batchSize
		if len(maArr) < end {
			end = len(maArr)
		}

		g.Go(func() error {
			select {
			case <-gCtx.Done():
				return nil
			default:
			}
			return saveFailedAttestationBatch(maArr[start:end], day)
		})
	}

	if err := g.Wait(); err != nil {
		logrus.Error(err)
		return err
	}
	logger.Infof("export completed, took %v", time.Since(start))

	if err := markColumnExported(day, "failed_attestations_exported"); err != nil {
		return err
	}

	logger.Infof("'failed attestation' statistics export of day %v completed, took %v", day, time.Since(exportStart))
	return nil
}

func saveFailedAttestationBatch(batch []*types.ValidatorFailedAttestationsStatistic, day uint64) error {
	var failedAttestationBatchNumArgs int = 4
	batchSize := len(batch)
	valueStrings := make([]string, 0, failedAttestationBatchNumArgs)
	valueArgs := make([]interface{}, 0, batchSize*failedAttestationBatchNumArgs)

	for i, stat := range batch {
		valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d, $%d)", i*failedAttestationBatchNumArgs+1, i*failedAttestationBatchNumArgs+2, i*failedAttestationBatchNumArgs+3, i*failedAttestationBatchNumArgs+4))
		valueArgs = append(valueArgs, stat.Index)
		valueArgs = append(valueArgs, day)
		valueArgs = append(valueArgs, stat.MissedAttestations)
		valueArgs = append(valueArgs, stat.OrphanedAttestations)
	}
	stmt := fmt.Sprintf(`
		insert into validator_stats (validatorindex, day, missed_attestations, orphaned_attestations) VALUES
		%s
		on conflict (validatorindex, day) do update set missed_attestations = excluded.missed_attestations, orphaned_attestations = excluded.orphaned_attestations;`,
		strings.Join(valueStrings, ","))
	_, err := WriterDb.Exec(stmt, valueArgs...)
	if err != nil {
		logrus.Errorf("Error inserting 'failed attestations' %v", err)
		return err
	}

	return nil
}

func markColumnExported(day uint64, column string) error {
	start := time.Now()
	logger.Infof("marking [%v] exported for day [%v] as completed in the status table", column, day)

	_, err := WriterDb.Exec(fmt.Sprintf(`	
		INSERT INTO validator_stats_status (day, status, %[1]v) 
		VALUES ($1, false, true) 
		ON CONFLICT (day) 
			DO UPDATE SET %[1]v=EXCLUDED.%[1]v;
			`, column), day)
	if err != nil {
		return err
	}
	logrus.Infof("Marking complete in %v", time.Since(start))
	return nil
}

func GetValidatorIncomeHistoryChart(validatorIndices []uint64, currency string, lastFinalizedEpoch uint64) ([]*types.ChartDataPoint, error) {
	incomeHistory, err := GetValidatorIncomeHistory(validatorIndices, 0, 0, lastFinalizedEpoch)
	if err != nil {
		return nil, err
	}
	var clRewardsSeries = make([]*types.ChartDataPoint, len(incomeHistory))

	for i := 0; i < len(incomeHistory); i++ {
		color := "#7cb5ec"
		if incomeHistory[i].ClRewards < 0 {
			color = "#f7a35c"
		}
		balanceTs := utils.DayToTime(incomeHistory[i].Day)
		clRewardsSeries[i] = &types.ChartDataPoint{X: float64(balanceTs.Unix() * 1000), Y: utils.ExchangeRateForCurrency(currency) * (float64(incomeHistory[i].ClRewards) / 1e9), Color: color}
	}
	return clRewardsSeries, err
}

func GetValidatorIncomeHistory(validatorIndices []uint64, lowerBoundDay uint64, upperBoundDay uint64, lastFinalizedEpoch uint64) ([]types.ValidatorIncomeHistory, error) {
	if len(validatorIndices) == 0 {
		return []types.ValidatorIncomeHistory{}, nil
	}

	if upperBoundDay == 0 {
		upperBoundDay = 65536
	}

	validatorIndices = utils.SortedUniqueUint64(validatorIndices)
	validatorIndicesStr := make([]string, len(validatorIndices))
	for i, v := range validatorIndices {
		validatorIndicesStr[i] = fmt.Sprintf("%d", v)
	}

	validatorIndicesPqArr := pq.Array(validatorIndices)

	cacheDur := time.Second * time.Duration(utils.Config.Chain.Config.SecondsPerSlot*utils.Config.Chain.Config.SlotsPerEpoch+10) // updates every epoch, keep 10sec longer
	cacheKey := fmt.Sprintf("%d:validatorIncomeHistory:%d:%d:%d:%s", utils.Config.Chain.Config.DepositChainID, lowerBoundDay, upperBoundDay, lastFinalizedEpoch, strings.Join(validatorIndicesStr, ","))
	if cached, err := cache.TieredCache.GetWithLocalTimeout(cacheKey, cacheDur, []types.ValidatorIncomeHistory{}); err == nil {
		return cached.([]types.ValidatorIncomeHistory), nil
	}

	var result []types.ValidatorIncomeHistory
	err := ReaderDb.Select(&result, `
		SELECT 
			day, 
			SUM(COALESCE(cl_rewards_gwei, 0)) AS cl_rewards_gwei,
			SUM(COALESCE(end_balance, 0)) AS end_balance
		FROM validator_stats 
		WHERE validatorindex = ANY($1) AND day BETWEEN $2 AND $3 
		GROUP BY day 
		ORDER BY day
	;`, validatorIndicesPqArr, lowerBoundDay, upperBoundDay)
	if err != nil {
		return nil, err
	}

	// retrieve rewards for epochs not yet in stats
	if upperBoundDay == 65536 {
		lastDay := uint64(0)
		if len(result) > 0 {
			lastDay = uint64(result[len(result)-1].Day)
		} else {
			lastDay, err = GetLastExportedStatisticDay()
			if err != nil {
				return nil, err
			}
		}

		currentDay := lastDay + 1
		firstEpoch := currentDay * utils.EpochsPerDay()

		totalBalance := uint64(0)

		g := errgroup.Group{}
		g.Go(func() error {
			latestBalances, err := BigtableClient.GetValidatorBalanceHistory(validatorIndices, lastFinalizedEpoch, lastFinalizedEpoch)
			if err != nil {
				logger.Errorf("error getting validator balance data in GetValidatorEarnings: %v", err)
				return err
			}

			for _, balance := range latestBalances {
				if len(balance) == 0 {
					continue
				}

				totalBalance += balance[0].Balance
			}
			return nil
		})

		var lastBalance uint64
		g.Go(func() error {
			return GetValidatorBalanceForDay(validatorIndices, lastDay, &lastBalance)
		})

		var lastDeposits uint64
		g.Go(func() error {
			return GetValidatorDepositsForEpochs(validatorIndices, firstEpoch, lastFinalizedEpoch, &lastDeposits)
		})

		var lastWithdrawals uint64
		g.Go(func() error {
			return GetValidatorWithdrawalsForEpochs(validatorIndices, firstEpoch, lastFinalizedEpoch, &lastWithdrawals)
		})

		err = g.Wait()
		if err != nil {
			return nil, err
		}

		result = append(result, types.ValidatorIncomeHistory{
			Day:       int64(currentDay),
			ClRewards: int64(totalBalance - lastBalance - lastDeposits + lastWithdrawals),
		})
	}

	go func() {
		err := cache.TieredCache.Set(cacheKey, result, cacheDur)
		if err != nil {
			utils.LogError(err, fmt.Errorf("error setting tieredCache for GetValidatorIncomeHistory with key %v", cacheKey), 0)
		}
	}()

	return result, nil
}

func WriteChartSeriesForDay(day int64) error {
	startTs := time.Now()

	if day < 0 {
		// before the beaconchain
		return fmt.Errorf("this function does not yet pre-beaconchain blocks")
	}

	epochsPerDay := utils.EpochsPerDay()
	beaconchainDay := day * int64(epochsPerDay)

	startDate := utils.EpochToTime(uint64(beaconchainDay))
	dateTrunc := time.Date(startDate.Year(), startDate.Month(), startDate.Day(), 0, 0, 0, 0, time.UTC)

	// inclusive slot
	firstSlot := utils.TimeToSlot(uint64(dateTrunc.Unix()))

	epochOffset := firstSlot % utils.Config.Chain.Config.SlotsPerEpoch
	firstSlot = firstSlot - epochOffset
	firstEpoch := firstSlot / utils.Config.Chain.Config.SlotsPerEpoch
	// exclusive slot
	lastSlot := int64(firstSlot) + int64(epochsPerDay*utils.Config.Chain.Config.SlotsPerEpoch)
	lastEpoch := lastSlot / int64(utils.Config.Chain.Config.SlotsPerEpoch)

	finalizedCount, err := CountFinalizedEpochs(firstEpoch, uint64(lastEpoch))
	if err != nil {
		return err
	}

	if finalizedCount < epochsPerDay {
		return fmt.Errorf("delaying chart series export as not all epochs for day %v finalized. %v of %v", day, finalizedCount, epochsPerDay)
	}

	firstBlock, err := GetBlockNumber(uint64(firstSlot))
	if err != nil {
		return fmt.Errorf("error getting block number for slot: %v err: %w", firstSlot, err)
	}

	if firstBlock <= 15537394 {
		return fmt.Errorf("this function does not yet handle pre merge statistics")
	}

	lastBlock, err := GetBlockNumber(uint64(lastSlot))
	if err != nil {
		return fmt.Errorf("error getting block number for slot: %v err: %w", lastSlot, err)
	}
	logger.Infof("exporting chart_series for day %v ts: %v (slot %v to %v, block %v to %v)", day, dateTrunc, firstSlot, lastSlot, firstBlock, lastBlock)

	blocksChan := make(chan *types.Eth1Block, 360)
	batchSize := int64(360)
	go func(stream chan *types.Eth1Block) {
		logger.Infof("querying blocks from %v to %v", firstBlock, lastBlock)
		for b := int64(lastBlock) - 1; b > int64(firstBlock); b -= batchSize {
			high := b
			low := b - batchSize + 1
			if int64(firstBlock) > low {
				low = int64(firstBlock)
			}

			err := BigtableClient.GetFullBlocksDescending(stream, uint64(high), uint64(low))
			if err != nil {
				logger.Errorf("error getting blocks descending high: %v low: %v err: %v", high, low, err)
			}

		}
		close(stream)
	}(blocksChan)

	// logger.Infof("got %v blocks", len(blocks))

	blockCount := int64(0)
	txCount := int64(0)

	totalBaseFee := decimal.NewFromInt(0)
	totalGasPrice := decimal.NewFromInt(0)
	totalTxSavings := decimal.NewFromInt(0)
	totalTxFees := decimal.NewFromInt(0)
	totalBurned := decimal.NewFromInt(0)
	totalGasUsed := decimal.NewFromInt(0)

	legacyTxCount := int64(0)
	accessListTxCount := int64(0)
	eip1559TxCount := int64(0)
	failedTxCount := int64(0)
	successTxCount := int64(0)

	totalFailedGasUsed := decimal.NewFromInt(0)
	totalFailedTxFee := decimal.NewFromInt(0)

	totalBaseBlockReward := decimal.NewFromInt(0)

	totalGasLimit := decimal.NewFromInt(0)
	totalTips := decimal.NewFromInt(0)

	// totalSize := decimal.NewFromInt(0)

	// blockCount := len(blocks)

	// missedBlockCount := (firstSlot - uint64(lastSlot)) - uint64(blockCount)

	var prevBlock *types.Eth1Block

	accumulatedBlockTime := decimal.NewFromInt(0)

	for blk := range blocksChan {
		// logger.Infof("analyzing block: %v with: %v transactions", blk.Number, len(blk.Transactions))
		blockCount += 1
		baseFee := decimal.NewFromBigInt(new(big.Int).SetBytes(blk.BaseFee), 0)
		totalBaseFee = totalBaseFee.Add(baseFee)
		totalGasLimit = totalGasLimit.Add(decimal.NewFromInt(int64(blk.GasLimit)))

		if prevBlock != nil {
			accumulatedBlockTime = accumulatedBlockTime.Add(decimal.NewFromInt(prevBlock.Time.AsTime().UnixMicro() - blk.Time.AsTime().UnixMicro()))
		}

		totalBaseBlockReward = totalBaseBlockReward.Add(decimal.NewFromBigInt(utils.Eth1BlockReward(blk.Number, blk.Difficulty), 0))

		for _, tx := range blk.Transactions {
			// for _, itx := range tx.Itx {
			// }
			// blk.Time
			txCount += 1
			maxFee := decimal.NewFromBigInt(new(big.Int).SetBytes(tx.MaxFeePerGas), 0)
			prioFee := decimal.NewFromBigInt(new(big.Int).SetBytes(tx.MaxPriorityFeePerGas), 0)
			gasUsed := decimal.NewFromBigInt(new(big.Int).SetUint64(tx.GasUsed), 0)
			gasPrice := decimal.NewFromBigInt(new(big.Int).SetBytes(tx.GasPrice), 0)

			var tipFee decimal.Decimal
			var txFees decimal.Decimal
			switch tx.Type {
			case 0:
				legacyTxCount += 1
				totalGasPrice = totalGasPrice.Add(gasPrice)
				txFees = gasUsed.Mul(gasPrice)
				tipFee = gasPrice.Sub(baseFee)

			case 1:
				accessListTxCount += 1
				totalGasPrice = totalGasPrice.Add(gasPrice)
				txFees = gasUsed.Mul(gasPrice)
				tipFee = gasPrice.Sub(baseFee)

			case 2:
				// priority fee is capped because the base fee is filled first
				tipFee = decimal.Min(prioFee, maxFee.Sub(baseFee))
				eip1559TxCount += 1
				// totalMinerTips = totalMinerTips.Add(tipFee.Mul(gasUsed))
				txFees = baseFee.Mul(gasUsed).Add(tipFee.Mul(gasUsed))
				totalTxSavings = totalTxSavings.Add(maxFee.Mul(gasUsed).Sub(baseFee.Mul(gasUsed).Add(tipFee.Mul(gasUsed))))

			default:
				logger.Fatalf("error unknown tx type %v hash: %x", tx.Status, tx.Hash)
			}
			totalTxFees = totalTxFees.Add(txFees)

			switch tx.Status {
			case 0:
				failedTxCount += 1
				totalFailedGasUsed = totalFailedGasUsed.Add(gasUsed)
				totalFailedTxFee = totalFailedTxFee.Add(txFees)
			case 1:
				successTxCount += 1
			default:
				logger.Fatalf("error unknown status code %v hash: %x", tx.Status, tx.Hash)
			}
			totalGasUsed = totalGasUsed.Add(gasUsed)
			totalBurned = totalBurned.Add(baseFee.Mul(gasUsed))
			if blk.Number < 12244000 {
				totalTips = totalTips.Add(gasUsed.Mul(gasPrice))
			} else {
				totalTips = totalTips.Add(gasUsed.Mul(tipFee))
			}
		}
		prevBlock = blk
	}

	avgBlockTime := accumulatedBlockTime.Div(decimal.NewFromInt(blockCount - 1))

	logger.Infof("exporting consensus rewards from %v to %v", firstEpoch, lastEpoch)

	// consensus rewards are in Gwei
	totalConsensusRewards := int64(0)

	err = WriterDb.Get(&totalConsensusRewards, "SELECT SUM(COALESCE(cl_rewards_gwei, 0)) FROM validator_stats WHERE day = $1", day)
	if err != nil {
		return fmt.Errorf("error calculating totalConsensusRewards: %w", err)
	}
	logger.Infof("consensus rewards: %v", totalConsensusRewards)

	logger.Infof("Exporting BURNED_FEES %v", totalBurned.String())
	_, err = WriterDb.Exec("INSERT INTO chart_series (time, indicator, value) VALUES ($1, 'BURNED_FEES', $2) ON CONFLICT (time, indicator) DO UPDATE SET value = EXCLUDED.value", dateTrunc, totalBurned.String())
	if err != nil {
		return fmt.Errorf("error calculating BURNED_FEES chart_series: %w", err)
	}

	logger.Infof("Exporting NON_FAILED_TX_GAS_USAGE %v", totalGasUsed.Sub(totalFailedGasUsed).String())
	err = SaveChartSeriesPoint(dateTrunc, "NON_FAILED_TX_GAS_USAGE", totalGasUsed.Sub(totalFailedGasUsed).String())
	if err != nil {
		return fmt.Errorf("error calculating NON_FAILED_TX_GAS_USAGE chart_series: %w", err)
	}
	logger.Infof("Exporting BLOCK_COUNT %v", blockCount)
	err = SaveChartSeriesPoint(dateTrunc, "BLOCK_COUNT", blockCount)
	if err != nil {
		return fmt.Errorf("error calculating BLOCK_COUNT chart_series: %w", err)
	}

	// convert microseconds to seconds
	logger.Infof("Exporting BLOCK_TIME_AVG %v", avgBlockTime.Div(decimal.NewFromInt(1e6)).Abs().String())
	err = SaveChartSeriesPoint(dateTrunc, "BLOCK_TIME_AVG", avgBlockTime.Div(decimal.NewFromInt(1e6)).String())
	if err != nil {
		return fmt.Errorf("error calculating BLOCK_TIME_AVG chart_series: %w", err)
	}
	// convert consensus rewards to gwei
	emission := (totalBaseBlockReward.Add(decimal.NewFromInt(totalConsensusRewards).Mul(decimal.NewFromInt(1000000000))).Add(totalTips)).Sub(totalBurned)
	logger.Infof("Exporting TOTAL_EMISSION %v day emission", emission)

	var lastEmission float64
	err = ReaderDb.Get(&lastEmission, "SELECT value FROM chart_series WHERE indicator = 'TOTAL_EMISSION' AND time < $1 ORDER BY time DESC LIMIT 1", dateTrunc)
	if err != nil {
		return fmt.Errorf("error getting previous value for TOTAL_EMISSION chart_series: %w", err)
	}

	newEmission := decimal.NewFromFloat(lastEmission).Add(emission)
	err = SaveChartSeriesPoint(dateTrunc, "TOTAL_EMISSION", newEmission)
	if err != nil {
		return fmt.Errorf("error calculating TOTAL_EMISSION chart_series: %w", err)
	}

	if totalGasPrice.GreaterThan(decimal.NewFromInt(0)) && decimal.NewFromInt(legacyTxCount).Add(decimal.NewFromInt(accessListTxCount)).GreaterThan(decimal.NewFromInt(0)) {
		logger.Infof("Exporting AVG_GASPRICE")
		_, err = WriterDb.Exec("INSERT INTO chart_series (time, indicator, value) VALUES($1, 'AVG_GASPRICE', $2) ON CONFLICT (time, indicator) DO UPDATE SET value = EXCLUDED.value", dateTrunc, totalGasPrice.Div((decimal.NewFromInt(legacyTxCount).Add(decimal.NewFromInt(accessListTxCount)))).String())
		if err != nil {
			return fmt.Errorf("error calculating AVG_GASPRICE chart_series err: %w", err)
		}
	}

	if txCount > 0 {
		logger.Infof("Exporting AVG_GASUSED %v", totalGasUsed.Div(decimal.NewFromInt(blockCount)).String())
		err = SaveChartSeriesPoint(dateTrunc, "AVG_GASUSED", totalGasUsed.Div(decimal.NewFromInt(blockCount)).String())
		if err != nil {
			return fmt.Errorf("error calculating AVG_GASUSED chart_series: %w", err)
		}
	}

	logger.Infof("Exporting TOTAL_GASUSED %v", totalGasUsed.String())
	err = SaveChartSeriesPoint(dateTrunc, "TOTAL_GASUSED", totalGasUsed.String())
	if err != nil {
		return fmt.Errorf("error calculating TOTAL_GASUSED chart_series: %w", err)
	}

	if blockCount > 0 {
		logger.Infof("Exporting AVG_GASLIMIT %v", totalGasLimit.Div(decimal.NewFromInt(blockCount)))
		err = SaveChartSeriesPoint(dateTrunc, "AVG_GASLIMIT", totalGasLimit.Div(decimal.NewFromInt(blockCount)))
		if err != nil {
			return fmt.Errorf("error calculating AVG_GASLIMIT chart_series: %w", err)
		}
	}

	if !totalGasLimit.IsZero() {
		logger.Infof("Exporting AVG_BLOCK_UTIL %v", totalGasUsed.Div(totalGasLimit).Mul(decimal.NewFromInt(100)))
		err = SaveChartSeriesPoint(dateTrunc, "AVG_BLOCK_UTIL", totalGasUsed.Div(totalGasLimit).Mul(decimal.NewFromInt(100)))
		if err != nil {
			return fmt.Errorf("error calculating AVG_BLOCK_UTIL chart_series: %w", err)
		}
	}

	logger.Infof("Exporting MARKET_CAP: %v", newEmission.Div(decimal.NewFromInt(1e18)).Add(decimal.NewFromFloat(72009990.50)).Mul(decimal.NewFromFloat(price.GetEthPrice("USD"))).String())
	err = SaveChartSeriesPoint(dateTrunc, "MARKET_CAP", newEmission.Div(decimal.NewFromInt(1e18)).Add(decimal.NewFromFloat(72009990.50)).Mul(decimal.NewFromFloat(price.GetEthPrice("USD"))).String())
	if err != nil {
		return fmt.Errorf("error calculating MARKET_CAP chart_series: %w", err)
	}

	logger.Infof("Exporting TX_COUNT %v", txCount)
	err = SaveChartSeriesPoint(dateTrunc, "TX_COUNT", txCount)
	if err != nil {
		return fmt.Errorf("error calculating TX_COUNT chart_series: %w", err)
	}

	// Not sure how this is currently possible (where do we store the size, i think this is missing)
	// logger.Infof("Exporting AVG_SIZE %v", totalSize.div)
	// err = SaveChartSeriesPoint(dateTrunc, "AVG_SIZE", totalSize.div)
	// if err != nil {
	// 	return fmt.Errorf("error calculating AVG_SIZE chart_series: %w", err)
	// }

	// logger.Infof("Exporting POWER_CONSUMPTION %v", avgBlockTime.String())
	// err = SaveChartSeriesPoint(dateTrunc, "POWER_CONSUMPTION", avgBlockTime.String())
	// if err != nil {
	// 	return fmt.Errorf("error calculating POWER_CONSUMPTION chart_series: %w", err)
	// }

	// logger.Infof("Exporting NEW_ACCOUNTS %v", avgBlockTime.String())
	// err = SaveChartSeriesPoint(dateTrunc, "NEW_ACCOUNTS", avgBlockTime.String())
	// if err != nil {
	// 	return fmt.Errorf("error calculating NEW_ACCOUNTS chart_series: %w", err)
	// }

	logger.Infof("marking day export as completed in the status table")
	_, err = WriterDb.Exec("insert into chart_series_status (day, status) values ($1, true)", day)
	if err != nil {
		return err
	}

	logger.Infof("chart_series export completed: took %v", time.Since(startTs))

	return nil
}

func checkIfDayIsFinalized(day uint64) error {
	epochsPerDay := utils.EpochsPerDay()
	firstEpoch, lastEpoch := utils.GetFirstAndLastEpochForDay(day)

	finalizedCount, err := CountFinalizedEpochs(firstEpoch, lastEpoch)
	if err != nil {
		return err
	}

	if finalizedCount < epochsPerDay {
		return fmt.Errorf("delaying export as not all epochs for day %v finalized. %v of %v", day, finalizedCount, epochsPerDay)
	}
	return nil
}
