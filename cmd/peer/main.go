/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package main

import (
	"fmt"
	"io/ioutil"
	_ "net/http/pprof"
	"os"
	"strings"
	"time"

	"github.com/gogo/protobuf/proto"
	cb "github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric/bccsp/factory"
	"github.com/hyperledger/fabric/common/configtx"
	"github.com/hyperledger/fabric/common/util"
	"github.com/hyperledger/fabric/internal/configtxgen/encoder"
	"github.com/hyperledger/fabric/internal/configtxgen/genesisconfig"
	"github.com/hyperledger/fabric/internal/peer/chaincode"
	"github.com/hyperledger/fabric/internal/peer/channel"
	"github.com/hyperledger/fabric/internal/peer/common"
	"github.com/hyperledger/fabric/internal/peer/lifecycle"
	"github.com/hyperledger/fabric/internal/peer/node"
	"github.com/hyperledger/fabric/internal/peer/snapshot"
	"github.com/hyperledger/fabric/internal/peer/version"
	"github.com/hyperledger/fabric/internal/pkg/identity"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// The main command describes the service and
// defaults to printing the help message.
var mainCmd = &cobra.Command{Use: "peer"}

func main() {
	// For environment variables.
	viper.SetEnvPrefix(common.CmdRoot)
	viper.AutomaticEnv()
	replacer := strings.NewReplacer(".", "_")
	viper.SetEnvKeyReplacer(replacer)

	// Define command-line flags that are valid for all peer commands and
	// subcommands.
	mainFlags := mainCmd.PersistentFlags()

	mainFlags.String("logging-level", "", "Legacy logging level flag")
	viper.BindPFlag("logging_level", mainFlags.Lookup("logging-level"))
	mainFlags.MarkHidden("logging-level")

	cryptoProvider := factory.GetDefault()

	mainCmd.AddCommand(version.Cmd())
	mainCmd.AddCommand(node.Cmd())
	mainCmd.AddCommand(chaincode.Cmd(nil, cryptoProvider))
	mainCmd.AddCommand(channel.Cmd(nil))
	mainCmd.AddCommand(lifecycle.Cmd(cryptoProvider))
	mainCmd.AddCommand(snapshot.Cmd(cryptoProvider))

	// On failure Cobra prints the usage message and error string, so we only
	// need to exit with a non-0 status
	if mainCmd.Execute() != nil {
		os.Exit(1)
	}
}

// ------------ Modification --------------------
var channelID string = "princechannel"
var timeout time.Duration

func Create(cmd *cobra.Command, args []string, cf *channel.ChannelCmdFactory) error {
	// logger.Info("---create_and_join: Create---")

	// the global chainID filled by the "-c" command
	var channelID = "princechannel2"
	if channelID == common.UndefinedParamValue {
		return errors.New("must supply channel ID")
	}

	// Parsing of the command line is done so silence cmd usage
	cmd.SilenceUsage = true

	var err error
	if cf == nil {
		cf, err = channel.InitCmdFactory(channel.EndorserNotRequired, channel.PeerDeliverNotRequired, channel.OrdererRequired)
		if err != nil {
			return err
		}
	}
	return executeCreate(cf)
}

func executeCreate(cf *channel.ChannelCmdFactory) error {
	// logger.Info("---executeCreate---")

	err := sendCreateChainTransaction(cf)
	if err != nil {
		return err
	}

	block, err := getGenesisBlock(cf)
	if err != nil {
		return err
	}

	b, err := proto.Marshal(block)
	if err != nil {
		return err
	}

	var channelID = "princechannel2"
	file := channelID + ".block"

	var outputBlock string
	if outputBlock != common.UndefinedParamValue {
		file = outputBlock
	}
	err = ioutil.WriteFile(file, b, 0o644)
	if err != nil {
		return err
	}

	return nil
}

func sendCreateChainTransaction(cf *channel.ChannelCmdFactory) error {
	// logger.Info("---sendCreateChainTransaction---")
	var err error
	var chCrtEnv *cb.Envelope

	var channelTxFile string
	if channelTxFile != "" {
		if chCrtEnv, err = createChannelFromConfigTx(channelTxFile); err != nil {
			return err
		}
	} else {
		if chCrtEnv, err = createChannelFromDefaults(cf); err != nil {
			return err
		}
	}

	if chCrtEnv, err = sanityCheckAndSignConfigTx(chCrtEnv, cf.Signer); err != nil {
		return err
	}

	var broadcastClient common.BroadcastClient
	broadcastClient, err = cf.BroadcastFactory()
	if err != nil {
		return errors.WithMessage(err, "error getting broadcast client")
	}

	defer broadcastClient.Close()
	err = broadcastClient.Send(chCrtEnv)

	return err
}

func sanityCheckAndSignConfigTx(envConfigUpdate *cb.Envelope, signer identity.SignerSerializer) (*cb.Envelope, error) {
	payload, err := protoutil.UnmarshalPayload(envConfigUpdate.Payload)
	if err != nil {
		return nil, channel.InvalidCreateTx("bad payload")
	}

	if payload.Header == nil || payload.Header.ChannelHeader == nil {
		return nil, channel.InvalidCreateTx("bad header")
	}

	ch, err := protoutil.UnmarshalChannelHeader(payload.Header.ChannelHeader)
	if err != nil {
		return nil, channel.InvalidCreateTx("could not unmarshall channel header")
	}

	if ch.Type != int32(cb.HeaderType_CONFIG_UPDATE) {
		return nil, channel.InvalidCreateTx("bad type")
	}

	if ch.ChannelId == "" {
		return nil, channel.InvalidCreateTx("empty channel id")
	}

	// Specifying the chainID on the CLI is usually redundant, as a hack, set it
	// here if it has not been set explicitly
	if channelID == "" {
		channelID = ch.ChannelId
	}

	if ch.ChannelId != channelID {
		return nil, channel.InvalidCreateTx(fmt.Sprintf("mismatched channel ID %s != %s", ch.ChannelId, channelID))
	}

	configUpdateEnv, err := configtx.UnmarshalConfigUpdateEnvelope(payload.Data)
	if err != nil {
		return nil, channel.InvalidCreateTx("Bad config update env")
	}

	sigHeader, err := protoutil.NewSignatureHeader(signer)
	if err != nil {
		return nil, err
	}

	configSig := &cb.ConfigSignature{
		SignatureHeader: protoutil.MarshalOrPanic(sigHeader),
	}

	configSig.Signature, err = signer.Sign(util.ConcatenateBytes(configSig.SignatureHeader, configUpdateEnv.ConfigUpdate))
	if err != nil {
		return nil, err
	}

	configUpdateEnv.Signatures = append(configUpdateEnv.Signatures, configSig)

	return protoutil.CreateSignedEnvelope(cb.HeaderType_CONFIG_UPDATE, channelID, signer, configUpdateEnv, 0, 0)
}

func createChannelFromDefaults(cf *channel.ChannelCmdFactory) (*cb.Envelope, error) {
	chCrtEnv, err := encoder.MakeChannelCreationTransaction(
		channelID,
		cf.Signer,
		genesisconfig.Load(genesisconfig.SampleSingleMSPChannelProfile),
	)
	if err != nil {
		return nil, err
	}

	return chCrtEnv, nil
}

func createChannelFromConfigTx(configTxFileName string) (*cb.Envelope, error) {
	cftx, err := ioutil.ReadFile(configTxFileName)
	if err != nil {
		return nil, channel.ConfigTxFileNotFound(err.Error())
	}

	return protoutil.UnmarshalEnvelope(cftx)
}

func getGenesisBlock(cf *channel.ChannelCmdFactory) (*cb.Block, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
			cf.DeliverClient.Close()
			return nil, errors.New("timeout waiting for channel creation")
		default:
			if block, err := cf.DeliverClient.GetSpecifiedBlock(0); err != nil {
				cf.DeliverClient.Close()
				cf, err = channel.InitCmdFactory(channel.EndorserNotRequired, channel.PeerDeliverNotRequired, channel.OrdererRequired)
				if err != nil {
					return nil, WithMessage(err, "failed connecting")
				}
				time.Sleep(200 * time.Millisecond)
			} else {
				cf.DeliverClient.Close()
				return block, nil
			}
		}
	}
}

// WithMessage annotates err with a new message.
// If err is nil, WithMessage returns nil.
func WithMessage(err error, message string) error {
	if err == nil {
		return nil
	}
	return &withMessage{
		cause: err,
		msg:   message,
	}
}

type withMessage struct {
	cause error
	msg   string
}

func (w *withMessage) Error() string { return w.msg + ": " + w.cause.Error() }
func (w *withMessage) Cause() error  { return w.cause }