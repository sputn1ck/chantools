package main

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil/hdkeychain"
	"github.com/guggero/chantools/btc"
	"github.com/guggero/chantools/dataformat"
	"github.com/guggero/chantools/lnd"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/spf13/cobra"
)

const (
	defaultFeeSatPerVByte = 2
	defaultCsvLimit       = 2016
)

type sweepTimeLockCommand struct {
	ApiURL      string
	Publish     bool
	SweepAddr   string
	MaxCsvLimit uint16
	FeeRate     uint16

	rootKey *rootKey
	inputs *inputFlags
	cmd    *cobra.Command
}

func newSweepTimeLockCommand() *cobra.Command {
	cc := &sweepTimeLockCommand{}
	cc.cmd = &cobra.Command{
		Use: "sweeptimelock",
		Short: "Sweep the force-closed state after the time lock has " +
			"expired",
		RunE: cc.Execute,
	}
	cc.cmd.Flags().StringVar(
		&cc.ApiURL, "apiurl", defaultAPIURL, "API URL to use (must "+
			"be esplora compatible)",
	)
	cc.cmd.Flags().BoolVar(
		&cc.Publish, "publish", false, "publish sweep TX to the chain "+
			"API instead of just printing the TX",
	)
	cc.cmd.Flags().StringVar(
		&cc.SweepAddr, "sweepaddr", "", "address to sweep the funds to",
	)
	cc.cmd.Flags().Uint16Var(
		&cc.MaxCsvLimit, "maxcsvlimit", defaultCsvLimit, "maximum CSV "+
			"limit to use",
	)
	cc.cmd.Flags().Uint16Var(
		&cc.FeeRate, "feerate", defaultFeeSatPerVByte, "fee rate to "+
			"use for the sweep transaction in sat/vByte",
	)

	cc.rootKey = newRootKey(cc.cmd, "deriving keys")
	cc.inputs = newInputFlags(cc.cmd)

	return cc.cmd
}

func (c *sweepTimeLockCommand) Execute(_ *cobra.Command, _ []string) error {
	extendedKey, err := c.rootKey.read()
	if err != nil {
		return fmt.Errorf("error reading root key: %v", err)
	}

	// Make sure sweep addr is set.
	if c.SweepAddr == "" {
		return fmt.Errorf("sweep addr is required")
	}

	// Parse channel entries from any of the possible input files.
	entries, err := c.inputs.parseInputType()
	if err != nil {
		return err
	}

	// Set default values.
	if c.MaxCsvLimit == 0 {
		c.MaxCsvLimit = defaultCsvLimit
	}
	if c.FeeRate == 0 {
		c.FeeRate = defaultFeeSatPerVByte
	}
	return sweepTimeLock(
		extendedKey, c.ApiURL, entries, c.SweepAddr, c.MaxCsvLimit,
		c.Publish, c.FeeRate,
	)
}

func sweepTimeLock(extendedKey *hdkeychain.ExtendedKey, apiURL string,
	entries []*dataformat.SummaryEntry, sweepAddr string,
	maxCsvTimeout uint16, publish bool, feeRate uint16) error {

	// Create signer and transaction template.
	signer := &lnd.Signer{
		ExtendedKey: extendedKey,
		ChainParams: chainParams,
	}
	api := &btc.ExplorerAPI{BaseURL: apiURL}

	sweepTx := wire.NewMsgTx(2)
	totalOutputValue := int64(0)
	signDescs := make([]*input.SignDescriptor, 0)
	var estimator input.TxWeightEstimator

	for _, entry := range entries {
		// Skip entries that can't be swept.
		if entry.ForceClose == nil ||
			(entry.ClosingTX != nil && entry.ClosingTX.AllOutsSpent) ||
			entry.LocalBalance == 0 {

			log.Infof("Not sweeping %s, info missing or all spent",
				entry.ChannelPoint)
			continue
		}

		fc := entry.ForceClose

		// Find index of sweepable output of commitment TX.
		txindex := -1
		if len(fc.Outs) == 1 {
			txindex = 0
			if fc.Outs[0].Value != entry.LocalBalance {
				log.Errorf("Potential value mismatch! %d vs "+
					"%d (%s)",
					fc.Outs[0].Value, entry.LocalBalance,
					entry.ChannelPoint)
			}
		} else {
			for idx, out := range fc.Outs {
				if out.Value == entry.LocalBalance {
					txindex = idx
				}
			}
		}
		if txindex == -1 {
			log.Errorf("Could not find sweep output for chan %s",
				entry.ChannelPoint)
			continue
		}

		// Prepare sweep script parameters.
		commitPoint, err := pubKeyFromHex(fc.CommitPoint)
		if err != nil {
			return fmt.Errorf("error parsing commit point: %v", err)
		}
		revBase, err := pubKeyFromHex(fc.RevocationBasePoint.PubKey)
		if err != nil {
			return fmt.Errorf("error parsing commit point: %v", err)
		}
		delayDesc := fc.DelayBasePoint.Desc()
		delayPrivKey, err := signer.FetchPrivKey(delayDesc)
		if err != nil {
			return fmt.Errorf("error getting private key: %v", err)
		}
		delayBase := delayPrivKey.PubKey()

		lockScript, err := hex.DecodeString(fc.Outs[txindex].Script)
		if err != nil {
			return fmt.Errorf("error parsing target script: %v",
				err)
		}

		// We can't rely on the CSV delay of the channel DB to be
		// correct. But it doesn't cost us a lot to just brute force it.
		csvTimeout, script, scriptHash, err := bruteForceDelay(
			input.TweakPubKey(delayBase, commitPoint),
			input.DeriveRevocationPubkey(revBase, commitPoint),
			lockScript, maxCsvTimeout,
		)
		if err != nil {
			log.Errorf("Could not create matching script for %s "+
				"or csv too high: %v", entry.ChannelPoint,
				err)
			continue
		}

		// Create the transaction input.
		txHash, err := chainhash.NewHashFromStr(fc.TXID)
		if err != nil {
			return fmt.Errorf("error parsing tx hash: %v", err)
		}
		sweepTx.TxIn = append(sweepTx.TxIn, &wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash:  *txHash,
				Index: uint32(txindex),
			},
			Sequence: input.LockTimeToSequence(
				false, uint32(csvTimeout),
			),
		})

		// Create the sign descriptor for the input.
		signDesc := &input.SignDescriptor{
			KeyDesc: *delayDesc,
			SingleTweak: input.SingleTweakBytes(
				commitPoint, delayBase,
			),
			WitnessScript: script,
			Output: &wire.TxOut{
				PkScript: scriptHash,
				Value:    int64(fc.Outs[txindex].Value),
			},
			HashType: txscript.SigHashAll,
		}
		totalOutputValue += int64(fc.Outs[txindex].Value)
		signDescs = append(signDescs, signDesc)

		// Account for the input weight.
		estimator.AddWitnessInput(input.ToLocalTimeoutWitnessSize)
	}

	// Add our sweep destination output.
	sweepScript, err := lnd.GetP2WPKHScript(sweepAddr, chainParams)
	if err != nil {
		return err
	}
	estimator.AddP2WKHOutput()

	// Calculate the fee based on the given fee rate and our weight
	// estimation.
	feeRateKWeight := chainfee.SatPerKVByte(1000 * feeRate).FeePerKWeight()
	totalFee := feeRateKWeight.FeeForWeight(int64(estimator.Weight()))

	log.Infof("Fee %d sats of %d total amount (estimated weight %d)",
		totalFee, totalOutputValue, estimator.Weight())

	sweepTx.TxOut = []*wire.TxOut{{
		Value:    totalOutputValue - int64(totalFee),
		PkScript: sweepScript,
	}}

	// Sign the transaction now.
	sigHashes := txscript.NewTxSigHashes(sweepTx)
	for idx, desc := range signDescs {
		desc.SigHashes = sigHashes
		desc.InputIndex = idx
		witness, err := input.CommitSpendTimeout(signer, desc, sweepTx)
		if err != nil {
			return err
		}
		sweepTx.TxIn[idx].Witness = witness
	}

	var buf bytes.Buffer
	err = sweepTx.Serialize(&buf)
	if err != nil {
		return err
	}

	// Publish TX.
	if publish {
		response, err := api.PublishTx(
			hex.EncodeToString(buf.Bytes()),
		)
		if err != nil {
			return err
		}
		log.Infof("Published TX %s, response: %s",
			sweepTx.TxHash().String(), response)
	}

	log.Infof("Transaction: %x", buf.Bytes())
	return nil
}

func pubKeyFromHex(pubKeyHex string) (*btcec.PublicKey, error) {
	pointBytes, err := hex.DecodeString(pubKeyHex)
	if err != nil {
		return nil, fmt.Errorf("error hex decoding pub key: %v", err)
	}
	return btcec.ParsePubKey(pointBytes, btcec.S256())
}

func bruteForceDelay(delayPubkey, revocationPubkey *btcec.PublicKey,
	targetScript []byte, maxCsvTimeout uint16) (int32, []byte, []byte,
	error) {

	if len(targetScript) != 34 {
		return 0, nil, nil, fmt.Errorf("invalid target script: %s",
			targetScript)
	}
	for i := uint16(0); i <= maxCsvTimeout; i++ {
		s, err := input.CommitScriptToSelf(
			uint32(i), delayPubkey, revocationPubkey,
		)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("error creating "+
				"script: %v", err)
		}
		sh, err := input.WitnessScriptHash(s)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("error hashing script: "+
				"%v", err)
		}
		if bytes.Equal(targetScript[0:8], sh[0:8]) {
			return int32(i), s, sh, nil
		}
	}
	return 0, nil, nil, fmt.Errorf("csv timeout not found for target "+
		"script %s", targetScript)
}
