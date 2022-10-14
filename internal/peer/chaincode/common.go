/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package chaincode

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric-chaincode-go/shim"

	// pcommon "github.com/hyperledger/fabric-protos-go/common"
	cb "github.com/hyperledger/fabric-protos-go/common"
	// pcommon "github.com/hyperledger/fabric-protos-go/common"
	ab "github.com/hyperledger/fabric-protos-go/orderer"
	pb "github.com/hyperledger/fabric-protos-go/peer"
	lb "github.com/hyperledger/fabric-protos-go/peer/lifecycle"
	"github.com/hyperledger/fabric/bccsp"
	"github.com/hyperledger/fabric/bccsp/factory"
	"github.com/hyperledger/fabric/cmd/bjit"
	"github.com/hyperledger/fabric/common/policydsl"
	"github.com/hyperledger/fabric/common/util"
	"github.com/hyperledger/fabric/core/chaincode/persistence"
	"github.com/hyperledger/fabric/core/container/externalbuilder"
	"github.com/hyperledger/fabric/internal/configtxgen/genesisconfig"
	"github.com/hyperledger/fabric/internal/peer/channel"
	"github.com/hyperledger/fabric/internal/peer/common"
	"github.com/hyperledger/fabric/internal/peer/packaging"

	// "github.com/hyperledger/fabric/internal/peer/lifecycle/chaincode"
	"github.com/hyperledger/fabric/internal/pkg/identity"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"google.golang.org/grpc"
	// "github.com/hyperledger/fabric/internal/peer/lifecycle/chaincode"
)

// checkSpec to see if chaincode resides within current package capture for language.
func checkSpec(spec *pb.ChaincodeSpec) error {
	// Don't allow nil value
	if spec == nil {
		return errors.New("expected chaincode specification, nil received")
	}
	if spec.ChaincodeId == nil {
		return errors.New("expected chaincode ID, nil received")
	}

	return platformRegistry.ValidateSpec(spec.Type.String(), spec.ChaincodeId.Path)
}

// getChaincodeDeploymentSpec get chaincode deployment spec given the chaincode spec
func getChaincodeDeploymentSpec(spec *pb.ChaincodeSpec, crtPkg bool) (*pb.ChaincodeDeploymentSpec, error) {
	var codePackageBytes []byte
	if crtPkg {
		var err error
		if err = checkSpec(spec); err != nil {
			return nil, err
		}

		codePackageBytes, err = platformRegistry.GetDeploymentPayload(spec.Type.String(), spec.ChaincodeId.Path)
		if err != nil {
			return nil, errors.WithMessage(err, "error getting chaincode package bytes")
		}
		chaincodePath, err := platformRegistry.NormalizePath(spec.Type.String(), spec.ChaincodeId.Path)
		if err != nil {
			return nil, errors.WithMessage(err, "failed to normalize chaincode path")
		}
		spec.ChaincodeId.Path = chaincodePath
	}

	return &pb.ChaincodeDeploymentSpec{ChaincodeSpec: spec, CodePackage: codePackageBytes}, nil
}

var (
	newChannelID string
)

// getChaincodeSpec get chaincode spec from the cli cmd parameters
func getChaincodeSpec(cmd *cobra.Command) (*pb.ChaincodeSpec, error) {
	spec := &pb.ChaincodeSpec{}
	if err := checkChaincodeCmdParams(cmd); err != nil {
		// unset usage silence because it's a command line usage error
		cmd.SilenceUsage = false
		return spec, err
	}

	// Build the spec
	input := chaincodeInput{}
	if err := json.Unmarshal([]byte(chaincodeCtorJSON), &input); err != nil {
		return spec, errors.Wrap(err, "chaincode argument error")
	}
	input.IsInit = isInit

	chaincodeLang = strings.ToUpper(chaincodeLang)
	spec = &pb.ChaincodeSpec{
		Type:        pb.ChaincodeSpec_Type(pb.ChaincodeSpec_Type_value[chaincodeLang]),
		ChaincodeId: &pb.ChaincodeID{Path: chaincodePath, Name: chaincodeName, Version: chaincodeVersion},
		Input:       &input.ChaincodeInput,
	}

	return spec, nil
}

// chaincodeInput is wrapper around the proto defined ChaincodeInput message that
// is decorated with a custom JSON unmarshaller.
type chaincodeInput struct {
	pb.ChaincodeInput
}

// UnmarshalJSON converts the string-based REST/JSON input to
// the []byte-based current ChaincodeInput structure.
func (c *chaincodeInput) UnmarshalJSON(b []byte) error {
	sa := struct {
		Function string
		Args     []string
	}{}
	err := json.Unmarshal(b, &sa)
	if err != nil {
		return err
	}
	allArgs := sa.Args
	if sa.Function != "" {
		allArgs = append([]string{sa.Function}, sa.Args...)
	}
	c.Args = util.ToChaincodeArgs(allArgs...)
	return nil
}

type externalVMAdapter struct {
	detector *externalbuilder.Detector
}

// Author: Prince
// ------------------------ 1. Start for Package -----------------------------------
type PackagerCC struct {
	Command          *cobra.Command
	Input            *PackageInputCC
	PlatformRegistry PlatformRegistry
	Writer           Writer
}

func Package(p *PackagerCC) error {
	pr := packaging.NewRegistry(packaging.SupportedPlatforms...)

	p = &PackagerCC{
		PlatformRegistry: pr,
		Writer:           &persistence.FilesystemIO{},
	}

	p.Command = cmd

	args := []string{"basic.tar.gz"}
	return p.PackageChaincode(args)
}

type PlatformRegistry interface {
	GetDeploymentPayload(ccType, path string) ([]byte, error)
	NormalizePath(ccType, path string) (string, error)
}

// PackageChaincode packages a chaincode.
func (p *PackagerCC) PackageChaincode(args []string) error {
	if p.Command != nil {
		// Parsing of the command line is done so silence cmd usage
		p.Command.SilenceUsage = true
	}

	if len(args) != 1 {
		return errors.New("invalid number of args. expected only the output file")
	}
	p.setInput(args[0])

	return p.Package()
}

type PackageInputCC struct {
	OutputFile string
	Path       string
	Type       string
	Label      string
}

// Validate checks for the required inputs
func (p *PackageInputCC) Validate() error {
	if p.Path == "" {
		return errors.New("chaincode path must be specified")
	}
	if p.Type == "" {
		return errors.New("chaincode language must be specified")
	}
	if p.OutputFile == "" {
		return errors.New("output file must be specified")
	}
	if p.Label == "" {
		return errors.New("package label must be specified")
	}
	if err := persistence.ValidateLabel(p.Label); err != nil {
		return err
	}

	return nil
}

var (
	packageLabel string
)

func (p *PackagerCC) setInput(outputFile string) {
	chaincodePath = "../chaincode/asset-transfer-basic/chaincode-go/"
	chaincodeLang = "golang"
	packageLabel = "basic_1"

	p.Input = &PackageInputCC{
		OutputFile: outputFile,
		Path:       chaincodePath,
		Type:       chaincodeLang,
		Label:      packageLabel,
	}
}

// Package packages chaincodes into the package type,
// (.tar.gz) used by _lifecycle and writes it to disk
func (p *PackagerCC) Package() error {
	err := p.Input.Validate()
	if err != nil {
		return err
	}

	pkgTarGzBytes, err := p.getTarGzBytes()
	if err != nil {
		return err
	}

	dir, name := filepath.Split(p.Input.OutputFile)
	// if p.Input.OutputFile is only file name, dir becomes an empty string that creates problem
	// while invoking 'WriteFile' function below. So, irrespective, translate dir into absolute path
	if dir, err = filepath.Abs(dir); err != nil {
		return err
	}
	err = p.Writer.WriteFile(dir, name, pkgTarGzBytes)
	if err != nil {
		err = errors.Wrapf(err, "error writing chaincode package to %s", p.Input.OutputFile)
		logger.Error(err.Error())
		return err
	}

	return nil
}

func (p *PackagerCC) getTarGzBytes() ([]byte, error) {
	payload := bytes.NewBuffer(nil)
	gw := gzip.NewWriter(payload)
	tw := tar.NewWriter(gw)

	normalizedPath, err := p.PlatformRegistry.NormalizePath(strings.ToUpper(p.Input.Type), p.Input.Path)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to normalize chaincode path")
	}
	metadataBytes, err := toJSON(normalizedPath, p.Input.Type, p.Input.Label)
	if err != nil {
		return nil, err
	}
	err = writeBytesToPackage(tw, "metadata.json", metadataBytes)
	if err != nil {
		return nil, errors.Wrap(err, "error writing package metadata to tar")
	}

	codeBytes, err := p.PlatformRegistry.GetDeploymentPayload(strings.ToUpper(p.Input.Type), p.Input.Path)
	if err != nil {
		return nil, errors.WithMessage(err, "error getting chaincode bytes")
	}

	codePackageName := "code.tar.gz"

	err = writeBytesToPackage(tw, codePackageName, codeBytes)
	if err != nil {
		return nil, errors.Wrap(err, "error writing package code bytes to tar")
	}

	err = tw.Close()
	if err == nil {
		err = gw.Close()
	}
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create tar for chaincode")
	}

	return payload.Bytes(), nil
}

func writeBytesToPackage(tw *tar.Writer, name string, payload []byte) error {
	err := tw.WriteHeader(&tar.Header{
		Name: name,
		Size: int64(len(payload)),
		Mode: 0o100644,
	})
	if err != nil {
		return err
	}

	_, err = tw.Write(payload)
	if err != nil {
		return err
	}

	return nil
}

func toJSON(path, ccType, label string) ([]byte, error) {
	metadata := &PackageMetadata{
		Path:  path,
		Type:  ccType,
		Label: label,
	}

	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal chaincode package metadata into JSON")
	}

	return metadataBytes, nil
}

type PackageMetadata struct {
	Path  string `json:"path"`
	Type  string `json:"type"`
	Label string `json:"label"`
}

// ------------------------ 2. Start for Calculate Package ID ---------------------------
type PackageIDCalculator struct {
	Command *cobra.Command
	Input   *CalculatePackageIDInput
	Reader  Reader
	Writer  io.Writer
}

type Reader interface {
	ReadFile(string) ([]byte, error)
}

type CalculatePackageIDInput struct {
	PackageFile  string
	OutputFormat string
}

func CalculatePackageID(p *PackageIDCalculator) error {
	p = &PackageIDCalculator{
		Reader: &persistence.FilesystemIO{},
		Writer: os.Stdout,
	}
	p.Command = cmd
	args := []string{"basic.tar.gz"}

	return p.CalculatePackageID(args)
}

// PackageIDCalculator calculates the package ID for a packaged chaincode.
func (p *PackageIDCalculator) CalculatePackageID(args []string) error {
	if p.Command != nil {
		// Parsing of the command line is done so silence cmd usage
		p.Command.SilenceUsage = true
	}

	if len(args) != 1 {
		return errors.New("invalid number of args. expected only the packaged chaincode file")
	}
	p.setInput(args[0])

	return p.PackageID()
}

// Validate checks that the required parameters are provided
func (i *CalculatePackageIDInput) Validate() error {
	if i.PackageFile == "" {
		return errors.New("chaincode install package must be provided")
	}

	return nil
}

type CalculatePackageIDOutput struct {
	PackageID string `json:"package_id"`
}

// PackageID calculates the package ID for a packaged chaincode and print it.
func (p *PackageIDCalculator) PackageID() error {
	err := p.Input.Validate()
	if err != nil {
		return err
	}
	pkgBytes, err := p.Reader.ReadFile(p.Input.PackageFile)
	if err != nil {
		return errors.WithMessagef(err, "failed to read chaincode package at '%s'", p.Input.PackageFile)
	}

	metadata, _, err := persistence.ParseChaincodePackage(pkgBytes)
	if err != nil {
		return errors.WithMessage(err, "could not parse as a chaincode install package")
	}

	packageID := persistence.PackageID(metadata.Label, pkgBytes)

	if strings.ToLower(p.Input.OutputFormat) == "json" {
		output := CalculatePackageIDOutput{
			PackageID: packageID,
		}
		outputJson, err := json.MarshalIndent(&output, "", "\t")
		if err != nil {
			return errors.WithMessage(err, "failed to marshal output")
		}
		fmt.Fprintf(p.Writer, "%s\n", string(outputJson))
		return nil
	}

	packageId = packageID
	logger.Info("Printing package ID:", packageID)
	fmt.Fprintf(p.Writer, "%s\n", packageID)
	return nil
}

func (p *PackageIDCalculator) setInput(packageFile string) {
	p.Input = &CalculatePackageIDInput{
		PackageFile:  packageFile,
		OutputFormat: output,
	}
}

// ------------------------ 3. Start for Query Installed ---------------------------
// Installer holds the dependencies needed to install
// a chaincode.
type InstallerCC struct {
	Command        *cobra.Command
	EndorserClient EndorserClient
	Input          *InstallInput
	Reader         Reader
	Signer         Signer
}

func Install(i *InstallerCC) error {
	ccInput := &ClientConnectionsInput{
		CommandName:           "install",
		EndorserRequired:      true,
		PeerAddresses:         peerAddresses,
		TLSRootCertFiles:      tlsRootCertFiles,
		ConnectionProfilePath: connectionProfilePath,
		TargetPeer:            targetPeer,
		TLSEnabled:            viper.GetBool("peer.tls.enabled"),
	}

	c, err := NewClientConnections(ccInput, cryptoProvider)
	if err != nil {
		return err
	}

	// install is currently only supported for one peer so just use
	// the first endorser client
	i = &InstallerCC{
		Command:        cmd,
		EndorserClient: c.EndorserClients[0],
		Reader:         &persistence.FilesystemIO{},
		Signer:         c.Signer,
	}

	args := []string{"basic.tar.gz"}
	return i.InstallChaincode(args)
}

func (i *InstallerCC) setInput(args []string) {
	i.Input = &InstallInput{}

	if len(args) > 0 {
		i.Input.PackageFile = args[0]
	}
}

// Install installs a chaincode for use with _lifecycle.
func (i *InstallerCC) Install() error {
	err := i.Input.Validate()
	if err != nil {
		return err
	}

	pkgBytes, err := i.Reader.ReadFile(i.Input.PackageFile)
	if err != nil {
		return errors.WithMessagef(err, "failed to read chaincode package at '%s'", i.Input.PackageFile)
	}

	serializedSigner, err := i.Signer.Serialize()
	if err != nil {
		return errors.Wrap(err, "failed to serialize signer")
	}

	proposal, err := i.createInstallProposal(pkgBytes, serializedSigner)
	if err != nil {
		return err
	}

	signedProposal, err := signProposal(proposal, i.Signer)
	if err != nil {
		return errors.WithMessage(err, "failed to create signed proposal for chaincode install")
	}

	return i.submitInstallProposal(signedProposal)
}

// Validate checks that the required install parameters
// are provided.
func (i *InstallInput) Validate() error {
	if i.PackageFile == "" {
		return errors.New("chaincode install package must be provided")
	}

	return nil
}

// InstallChaincode installs the chaincode.
func (i *InstallerCC) InstallChaincode(args []string) error {
	if i.Command != nil {
		// Parsing of the command line is done so silence cmd usage
		i.Command.SilenceUsage = true
	}

	i.setInput(args)

	return i.Install()
}

func (i *InstallerCC) submitInstallProposal(signedProposal *pb.SignedProposal) error {
	proposalResponse, err := i.EndorserClient.ProcessProposal(context.Background(), signedProposal)
	if err != nil {
		return errors.WithMessage(err, "failed to endorse chaincode install")
	}

	if proposalResponse == nil {
		return errors.New("chaincode install failed: received nil proposal response")
	}

	if proposalResponse.Response == nil {
		return errors.New("chaincode install failed: received proposal response with nil response")
	}

	if proposalResponse.Response.Status != int32(cb.Status_SUCCESS) {
		return errors.Errorf("chaincode install failed with status: %d - %s", proposalResponse.Response.Status, proposalResponse.Response.Message)
	}
	logger.Infof("Installed remotely: %v", proposalResponse)

	icr := &lb.InstallChaincodeResult{}
	err = proto.Unmarshal(proposalResponse.Response.Payload, icr)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal proposal response's response payload")
	}
	logger.Infof("Chaincode code package identifier: %s", icr.PackageId)

	return nil
}

func (i *InstallerCC) createInstallProposal(pkgBytes []byte, creatorBytes []byte) (*pb.Proposal, error) {
	installChaincodeArgs := &lb.InstallChaincodeArgs{
		ChaincodeInstallPackage: pkgBytes,
	}

	installChaincodeArgsBytes, err := proto.Marshal(installChaincodeArgs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal InstallChaincodeArgs")
	}

	ccInput := &pb.ChaincodeInput{Args: [][]byte{[]byte("InstallChaincode"), installChaincodeArgsBytes}}

	cis := &pb.ChaincodeInvocationSpec{
		ChaincodeSpec: &pb.ChaincodeSpec{
			ChaincodeId: &pb.ChaincodeID{Name: lifecycleName},
			Input:       ccInput,
		},
	}

	proposal, _, err := protoutil.CreateProposalFromCIS(cb.HeaderType_ENDORSER_TRANSACTION, "", cis, creatorBytes)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to create proposal for ChaincodeInvocationSpec")
	}

	return proposal, nil
}

// ------------------------ 4. Start for Query Installed ---------------------------
type ClientConnectionsInput struct {
	CommandName           string
	EndorserRequired      bool
	OrdererRequired       bool
	OrderingEndpoint      string
	ChannelID             string
	PeerAddresses         []string
	TLSRootCertFiles      []string
	ConnectionProfilePath string
	TargetPeer            string
	TLSEnabled            bool
}

// Chaincode-related variables.
var (
	connectionProfilePath string
	targetPeer            string
	output                string
)

type InstalledQueryInput struct {
	OutputFormat string
}

type EndorserClient interface {
	ProcessProposal(ctx context.Context, in *pb.SignedProposal, opts ...grpc.CallOption) (*pb.ProposalResponse, error)
}

type Signer interface {
	Sign(msg []byte) ([]byte, error)
	Serialize() ([]byte, error)
}

type InstalledQuerier struct {
	Command        *cobra.Command
	Input          *InstalledQueryInput
	EndorserClient EndorserClient
	Signer         Signer
	Writer         io.Writer
}

var cryptoProvider = factory.GetDefault()
var cmd *cobra.Command

func QueryInstalled(i *InstalledQuerier) error {
	ccInput := &ClientConnectionsInput{
		CommandName:           "queryinstalled",
		EndorserRequired:      true,
		PeerAddresses:         peerAddresses,
		TLSRootCertFiles:      tlsRootCertFiles,
		ConnectionProfilePath: connectionProfilePath,
		TargetPeer:            targetPeer,
		TLSEnabled:            viper.GetBool("peer.tls.enabled"),
	}

	cc, err := NewClientConnections(ccInput, cryptoProvider)
	if err != nil {
		return err
	}

	iqInput := &InstalledQueryInput{
		OutputFormat: output,
	}

	// queryinstalled only supports one peer connection,
	// which is why we only wire in the first endorser
	// client
	i = &InstalledQuerier{
		Command:        cmd,
		EndorserClient: cc.EndorserClients[0],
		Input:          iqInput,
		Signer:         cc.Signer,
		Writer:         os.Stdout,
	}

	return i.Query()
}

type ClientConnections struct {
	BroadcastClient common.BroadcastClient
	DeliverClients  []pb.DeliverClient
	EndorserClients []pb.EndorserClient
	Certificate     tls.Certificate
	Signer          identity.SignerSerializer
	CryptoProvider  bccsp.BCCSP
}

// NewClientConnections creates a new set of client connections based on the
// input parameters.
func NewClientConnections(input *ClientConnectionsInput, cryptoProvider bccsp.BCCSP) (*ClientConnections, error) {
	signer, err := common.GetDefaultSigner()
	if err != nil {
		return nil, errors.WithMessage(err, "failed to retrieve default signer")
	}

	c := &ClientConnections{
		Signer:         signer,
		CryptoProvider: cryptoProvider,
	}

	if input.EndorserRequired {
		err := c.setPeerClients(input)
		if err != nil {
			return nil, err
		}
	}

	if input.OrdererRequired {
		err := c.setOrdererClient()
		if err != nil {
			return nil, err
		}
	}

	return c, nil
}

func (c *ClientConnections) setPeerClients(input *ClientConnectionsInput) error {
	var endorserClients []pb.EndorserClient
	var deliverClients []pb.DeliverClient

	if err := c.validatePeerConnectionParameters(input); err != nil {
		return errors.WithMessage(err, "failed to validate peer connection parameters")
	}

	for i, address := range input.PeerAddresses {
		var tlsRootCertFile string
		if input.TLSRootCertFiles != nil {
			tlsRootCertFile = input.TLSRootCertFiles[i]
		}
		endorserClient, err := common.GetEndorserClient(address, tlsRootCertFile)
		if err != nil {
			return errors.WithMessagef(err, "failed to retrieve endorser client for %s", input.CommandName)
		}
		endorserClients = append(endorserClients, endorserClient)
		deliverClient, err := common.GetPeerDeliverClient(address, tlsRootCertFile)
		if err != nil {
			return errors.WithMessagef(err, "failed to retrieve deliver client for %s", input.CommandName)
		}
		deliverClients = append(deliverClients, deliverClient)
	}
	if len(endorserClients) == 0 {
		// this should only be empty due to a programming bug
		return errors.New("no endorser clients retrieved")
	}

	err := c.setCertificate()
	if err != nil {
		return err
	}

	c.EndorserClients = endorserClients
	c.DeliverClients = deliverClients

	return nil
}

func (c *ClientConnections) validatePeerConnectionParameters(input *ClientConnectionsInput) error {
	if input.ConnectionProfilePath != "" {
		err := input.parseConnectionProfile()
		if err != nil {
			return err
		}
	}

	// currently only support multiple peer addresses for _lifecycle
	// for approveformyorg and commit
	multiplePeersAllowed := map[string]bool{
		"approveformyorg": true,
		"commit":          true,
	}
	if !multiplePeersAllowed[input.CommandName] && len(input.PeerAddresses) > 1 {
		return errors.Errorf("'%s' command supports one peer. %d peers provided", input.CommandName, len(input.PeerAddresses))
	}

	if !input.TLSEnabled {
		input.TLSRootCertFiles = nil
		return nil
	}
	if len(input.TLSRootCertFiles) != len(input.PeerAddresses) {
		return errors.Errorf("number of peer addresses (%d) does not match the number of TLS root cert files (%d)", len(input.PeerAddresses), len(input.TLSRootCertFiles))
	}

	return nil
}

func (c *ClientConnectionsInput) parseConnectionProfile() error {
	networkConfig, err := common.GetConfig(c.ConnectionProfilePath)
	if err != nil {
		return err
	}

	c.PeerAddresses = []string{}
	c.TLSRootCertFiles = []string{}

	if c.ChannelID == "" {
		if c.TargetPeer == "" {
			return errors.New("--targetPeer must be specified for channel-less operation using connection profile")
		}
		return c.appendPeerConfig(networkConfig, c.TargetPeer)
	}

	if len(networkConfig.Channels[c.ChannelID].Peers) == 0 {
		return nil
	}

	for peer, peerChannelConfig := range networkConfig.Channels[c.ChannelID].Peers {
		if peerChannelConfig.EndorsingPeer {
			err := c.appendPeerConfig(networkConfig, peer)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *ClientConnectionsInput) appendPeerConfig(n *common.NetworkConfig, peer string) error {
	peerConfig, ok := n.Peers[peer]
	if !ok {
		return errors.Errorf("peer '%s' doesn't have associated peer config", peer)
	}
	c.PeerAddresses = append(c.PeerAddresses, peerConfig.URL)
	c.TLSRootCertFiles = append(c.TLSRootCertFiles, peerConfig.TLSCACerts.Path)

	return nil
}

func (c *ClientConnections) setCertificate() error {
	certificate, err := common.GetClientCertificate()
	if err != nil {
		return errors.WithMessage(err, "failed to retrieve client cerificate")
	}

	c.Certificate = certificate

	return nil
}

func (c *ClientConnections) setOrdererClient() error {
	oe := viper.GetString("orderer.address")
	if oe == "" {
		// if we're here we didn't get an orderer endpoint from the command line
		// so we'll attempt to get one from cscc - bless it
		if c.Signer == nil {
			return errors.New("cannot obtain orderer endpoint, no signer was configured")
		}

		if len(c.EndorserClients) == 0 {
			return errors.New("cannot obtain orderer endpoint, empty endorser list")
		}

		orderingEndpoints, err := common.GetOrdererEndpointOfChainFnc(channelID, c.Signer, c.EndorserClients[0], c.CryptoProvider)
		if err != nil {
			return errors.WithMessagef(err, "error getting channel (%s) orderer endpoint", channelID)
		}
		if len(orderingEndpoints) == 0 {
			return errors.Errorf("no orderer endpoints retrieved for channel %s, pass orderer endpoint with -o flag instead", channelID)
		}

		logger.Infof("Retrieved channel (%s) orderer endpoint: %s", channelID, orderingEndpoints[0])
		// override viper env
		viper.Set("orderer.address", orderingEndpoints[0])
	}

	broadcastClient, err := common.GetBroadcastClient()
	if err != nil {
		return errors.WithMessage(err, "failed to retrieve broadcast client")
	}

	c.BroadcastClient = broadcastClient

	return nil
}

// Query returns the chaincodes installed on a peer
func (i *InstalledQuerier) Query() error {
	if i.Command != nil {
		// Parsing of the command line is done so silence cmd usage
		i.Command.SilenceUsage = true
	}

	proposal, err := i.createProposalForQuery()
	if err != nil {
		return errors.WithMessage(err, "failed to create proposal")
	}

	signedProposal, err := signProposal(proposal, i.Signer)
	if err != nil {
		return errors.WithMessage(err, "failed to create signed proposal")
	}

	proposalResponse, err := i.EndorserClient.ProcessProposal(context.Background(), signedProposal)
	if err != nil {
		return errors.WithMessage(err, "failed to endorse proposal")
	}

	if proposalResponse == nil {
		return errors.New("received nil proposal response")
	}

	if proposalResponse.Response == nil {
		return errors.New("received proposal response with nil response")
	}

	if proposalResponse.Response.Status != int32(cb.Status_SUCCESS) {
		return errors.Errorf("query failed with status: %d - %s", proposalResponse.Response.Status, proposalResponse.Response.Message)
	}

	if strings.ToLower(i.Input.OutputFormat) == "json" {
		return printResponseAsJSON(proposalResponse, &lb.QueryInstalledChaincodesResult{}, i.Writer)
	}
	return i.printResponse(proposalResponse)
}

// printResponse prints the information included in the response
// from the server.
func (i *InstalledQuerier) printResponse(proposalResponse *pb.ProposalResponse) error {
	qicr := &lb.QueryInstalledChaincodesResult{}
	err := proto.Unmarshal(proposalResponse.Response.Payload, qicr)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal proposal response's response payload")
	}
	fmt.Fprintln(i.Writer, "Installed chaincodes on peer:")
	for _, chaincode := range qicr.InstalledChaincodes {
		fmt.Fprintf(i.Writer, "Package ID: %s, Label: %s\n", chaincode.PackageId, chaincode.Label)
	}
	return nil
}

func printResponseAsJSON(proposalResponse *pb.ProposalResponse, msg proto.Message, out io.Writer) error {
	err := proto.Unmarshal(proposalResponse.Response.Payload, msg)
	if err != nil {
		return errors.Wrapf(err, "failed to unmarshal proposal response's response payload as type %T", msg)
	}

	bytes, err := json.MarshalIndent(msg, "", "\t")
	if err != nil {
		return errors.Wrap(err, "failed to marshal output")
	}

	fmt.Fprintf(out, "%s\n", string(bytes))

	return nil
}

func signProposal(proposal *pb.Proposal, signer Signer) (*pb.SignedProposal, error) {
	// check for nil argument
	if proposal == nil {
		return nil, errors.New("proposal cannot be nil")
	}

	if signer == nil {
		return nil, errors.New("signer cannot be nil")
	}

	proposalBytes, err := proto.Marshal(proposal)
	if err != nil {
		return nil, errors.Wrap(err, "error marshaling proposal")
	}

	signature, err := signer.Sign(proposalBytes)
	if err != nil {
		return nil, err
	}

	return &pb.SignedProposal{
		ProposalBytes: proposalBytes,
		Signature:     signature,
	}, nil
}

const (
	lifecycleName = "_lifecycle"
)

func (i *InstalledQuerier) createProposalForQuery() (*pb.Proposal, error) {
	args := &lb.QueryInstalledChaincodesArgs{}

	argsBytes, err := proto.Marshal(args)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal args")
	}

	ccInput := &pb.ChaincodeInput{
		Args: [][]byte{[]byte("QueryInstalledChaincodes"), argsBytes},
	}

	cis := &pb.ChaincodeInvocationSpec{
		ChaincodeSpec: &pb.ChaincodeSpec{
			ChaincodeId: &pb.ChaincodeID{Name: lifecycleName},
			Input:       ccInput,
		},
	}

	signerSerialized, err := i.Signer.Serialize()
	if err != nil {
		return nil, errors.WithMessage(err, "failed to serialize identity")
	}

	proposal, _, err := protoutil.CreateProposalFromCIS(cb.HeaderType_ENDORSER_TRANSACTION, "", cis, signerSerialized)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to create ChaincodeInvocationSpec proposal")
	}

	return proposal, nil
}

// ------------------------ End for Query Installed ---------------------------
// ------------------------ 5. Start for Get Query Installed Package ---------------------------
type GetInstalledPackageInput struct {
	PackageID       string
	OutputDirectory string
}

var (
	packageId       string
	outputDirectory string
)

type InstalledPackageGetter struct {
	Command        *cobra.Command
	Input          *GetInstalledPackageInput
	EndorserClient EndorserClient
	Signer         Signer
	Writer         Writer
}

type Writer interface {
	WriteFile(string, string, []byte) error
}

func GetInstalledPackage(i *InstalledPackageGetter) error {
	ccInput := &ClientConnectionsInput{
		CommandName:           "getqueryinstalledpkg",
		EndorserRequired:      true,
		PeerAddresses:         peerAddresses,
		TLSRootCertFiles:      tlsRootCertFiles,
		ConnectionProfilePath: connectionProfilePath,
		TargetPeer:            targetPeer,
		TLSEnabled:            viper.GetBool("peer.tls.enabled"),
	}

	cc, err := NewClientConnections(ccInput, cryptoProvider)
	if err != nil {
		return err
	}
	gipInput := &GetInstalledPackageInput{
		PackageID:       packageId,
		OutputDirectory: outputDirectory,
	}

	// getinstalledpackage only supports one peer connection,
	// which is why we only wire in the first endorser
	// client
	i = &InstalledPackageGetter{
		Command:        cmd,
		EndorserClient: cc.EndorserClients[0],
		Input:          gipInput,
		Signer:         cc.Signer,
		Writer:         &persistence.FilesystemIO{},
	}

	return i.Get()
}

// Validate checks that the required parameters are provided.
func (i *GetInstalledPackageInput) Validate() error {
	if i.PackageID == "" {
		return errors.New("The required parameter 'package-id' is empty. Rerun the command with --package-id flag")
	}

	return nil
}

// Get retrieves the installed chaincode package from a peer.
func (i *InstalledPackageGetter) Get() error {
	if i.Command != nil {
		// Parsing of the command line is done so silence cmd usage
		i.Command.SilenceUsage = true
	}

	if err := i.Input.Validate(); err != nil {
		return err
	}

	proposal, err := i.createProposalForPackage()
	if err != nil {
		return errors.WithMessage(err, "failed to create proposal")
	}

	signedProposal, err := signProposal(proposal, i.Signer)
	if err != nil {
		return errors.WithMessage(err, "failed to create signed proposal")
	}

	proposalResponse, err := i.EndorserClient.ProcessProposal(context.Background(), signedProposal)
	if err != nil {
		return errors.WithMessage(err, "failed to endorse proposal")
	}

	if proposalResponse == nil {
		return errors.New("received nil proposal response")
	}

	if proposalResponse.Response == nil {
		return errors.New("received proposal response with nil response")
	}

	if proposalResponse.Response.Status != int32(cb.Status_SUCCESS) {
		return errors.Errorf("proposal failed with status: %d - %s", proposalResponse.Response.Status, proposalResponse.Response.Message)
	}

	return i.writePackage(proposalResponse)
}

func (i *InstalledPackageGetter) writePackage(proposalResponse *pb.ProposalResponse) error {
	result := &lb.GetInstalledChaincodePackageResult{}
	err := proto.Unmarshal(proposalResponse.Response.Payload, result)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal proposal response's response payload")
	}

	outputFile := filepath.Join(i.Input.OutputDirectory, i.Input.PackageID+".tar.gz")

	dir, name := filepath.Split(outputFile)
	// translate dir into absolute path
	if dir, err = filepath.Abs(dir); err != nil {
		return err
	}

	err = i.Writer.WriteFile(dir, name, result.ChaincodeInstallPackage)
	if err != nil {
		err = errors.Wrapf(err, "failed to write chaincode package to %s", outputFile)
		logger.Error(err.Error())
		return err
	}

	return nil
}

func (i *InstalledPackageGetter) createProposalForPackage() (*pb.Proposal, error) {
	args := &lb.GetInstalledChaincodePackageArgs{
		PackageId: i.Input.PackageID,
	}

	argsBytes, err := proto.Marshal(args)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal args")
	}

	ccInput := &pb.ChaincodeInput{
		Args: [][]byte{[]byte("GetInstalledChaincodePackage"), argsBytes},
	}

	cis := &pb.ChaincodeInvocationSpec{
		ChaincodeSpec: &pb.ChaincodeSpec{
			ChaincodeId: &pb.ChaincodeID{Name: lifecycleName},
			Input:       ccInput,
		},
	}

	signerSerialized, err := i.Signer.Serialize()
	if err != nil {
		return nil, errors.WithMessage(err, "failed to serialize identity")
	}

	proposal, _, err := protoutil.CreateProposalFromCIS(cb.HeaderType_ENDORSER_TRANSACTION, "", cis, signerSerialized)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to create ChaincodeInvocationSpec proposal")
	}

	return proposal, nil
}

// approveformyorg

type ApproverForMyOrg struct {
	Certificate     tls.Certificate
	Command         *cobra.Command
	BroadcastClient common.BroadcastClient
	DeliverClients  []pb.DeliverClient
	EndorserClients []EndorserClient
	Input           *ApproveForMyOrgInput
	Signer          Signer
}

type ApproveForMyOrgInput struct {
	ChannelID                string
	Name                     string
	Version                  string
	PackageID                string
	Sequence                 int64
	EndorsementPlugin        string
	ValidationPlugin         string
	ValidationParameterBytes []byte
	CollectionConfigPackage  *pb.CollectionConfigPackage
	InitRequired             bool
	PeerAddresses            []string
	WaitForEvent             bool
	WaitForEventTimeout      time.Duration
	TxID                     string
}

func ApproveForMyOrg(a *ApproverForMyOrg) error {
	// a := &ApproverForMyOrg{}
	input, err := a.createInput()
	if err != nil {
		return err
	}

	// peerAddresses = []string{}
	// tlsRootCertFiles = []string{}

	ccInput := &ClientConnectionsInput{
		CommandName:           "approveformyorg",
		EndorserRequired:      true,
		OrdererRequired:       true,
		ChannelID:             newChannelID,
		PeerAddresses:         peerAddresses,
		TLSRootCertFiles:      tlsRootCertFiles,
		ConnectionProfilePath: connectionProfilePath,
		TLSEnabled:            viper.GetBool("peer.tls.enabled"),
	}

	cc, err := NewClientConnections(ccInput, cryptoProvider)
	if err != nil {
		return err
	}

	endorserClients := make([]EndorserClient, len(cc.EndorserClients))
	for i, e := range cc.EndorserClients {
		endorserClients[i] = e
	}

	a = &ApproverForMyOrg{
		Command:         cmd,
		Input:           input,
		Certificate:     cc.Certificate,
		BroadcastClient: cc.BroadcastClient,
		DeliverClients:  cc.DeliverClients,
		EndorserClients: endorserClients,
		Signer:          cc.Signer,
	}

	return a.Approve()
}

// Validate the input for an ApproveChaincodeDefinitionForMyOrg proposal
func (a *ApproveForMyOrgInput) Validate() error {
	if a.ChannelID == "" {
		return errors.New("The required parameter 'channelID' is empty. Rerun the command with -C flag")
	}

	if a.Name == "" {
		return errors.New("The required parameter 'name' is empty. Rerun the command with -n flag")
	}

	if a.Version == "" {
		return errors.New("The required parameter 'version' is empty. Rerun the command with -v flag")
	}

	if a.Sequence == 0 {
		return errors.New("The required parameter 'sequence' is empty. Rerun the command with --sequence flag")
	}

	return nil
}

// Approve submits a ApproveChaincodeDefinitionForMyOrg
// proposal
func (a *ApproverForMyOrg) Approve() error {
	err := a.Input.Validate()
	if err != nil {
		return err
	}

	if a.Command != nil {
		// Parsing of the command line is done so silence cmd usage
		a.Command.SilenceUsage = true
	}

	proposal, txID, err := a.createProposal(a.Input.TxID)
	if err != nil {
		return errors.WithMessage(err, "failed to create proposal")
	}

	signedProposal, err := signProposal(proposal, a.Signer)
	if err != nil {
		return errors.WithMessage(err, "failed to create signed proposal")
	}

	var responses []*pb.ProposalResponse
	for _, endorser := range a.EndorserClients {
		proposalResponse, err := endorser.ProcessProposal(context.Background(), signedProposal)
		if err != nil {
			return errors.WithMessage(err, "failed to endorse proposal")
		}
		responses = append(responses, proposalResponse)
	}

	if len(responses) == 0 {
		// this should only be empty due to a programming bug
		return errors.New("no proposal responses received")
	}

	// all responses will be checked when the signed transaction is created.
	// for now, just set this so we check the first response's status
	proposalResponse := responses[0]

	if proposalResponse == nil {
		return errors.New("received nil proposal response")
	}

	if proposalResponse.Response == nil {
		return errors.Errorf("received proposal response with nil response")
	}

	if proposalResponse.Response.Status != int32(cb.Status_SUCCESS) {
		return errors.Errorf("proposal failed with status: %d - %s", proposalResponse.Response.Status, proposalResponse.Response.Message)
	}
	// assemble a signed transaction (it's an Envelope message)
	env, err := protoutil.CreateSignedTx(proposal, a.Signer, responses...)
	if err != nil {
		return errors.WithMessage(err, "failed to create signed transaction")
	}
	var dg *DeliverGroup
	var ctx context.Context
	if a.Input.WaitForEvent {
		var cancelFunc context.CancelFunc
		ctx, cancelFunc = context.WithTimeout(context.Background(), a.Input.WaitForEventTimeout)
		defer cancelFunc()

		dg = NewDeliverGroup(
			a.DeliverClients,
			a.Input.PeerAddresses,
			a.Signer,
			a.Certificate,
			a.Input.ChannelID,
			txID,
		)
		// connect to deliver service on all peers
		err := dg.Connect(ctx)
		if err != nil {
			return err
		}
	}

	if err = a.BroadcastClient.Send(env); err != nil {
		return errors.WithMessage(err, "failed to send transaction")
	}

	if dg != nil && ctx != nil {
		// wait for event that contains the txID from all peers
		err = dg.Wait(ctx)
		if err != nil {
			return err
		}
	}

	return err
}

const (
	approveFuncName = "ApproveChaincodeDefinitionForMyOrg"
)

func (a *ApproverForMyOrg) createProposal(inputTxID string) (proposal *pb.Proposal, txID string, err error) {
	if a.Signer == nil {
		return nil, "", errors.New("nil signer provided")
	}

	var ccsrc *lb.ChaincodeSource
	if a.Input.PackageID != "" {
		ccsrc = &lb.ChaincodeSource{
			Type: &lb.ChaincodeSource_LocalPackage{
				LocalPackage: &lb.ChaincodeSource_Local{
					PackageId: a.Input.PackageID,
				},
			},
		}
	} else {
		ccsrc = &lb.ChaincodeSource{
			Type: &lb.ChaincodeSource_Unavailable_{
				Unavailable: &lb.ChaincodeSource_Unavailable{},
			},
		}
	}

	args := &lb.ApproveChaincodeDefinitionForMyOrgArgs{
		Name:                a.Input.Name,
		Version:             a.Input.Version,
		Sequence:            a.Input.Sequence,
		EndorsementPlugin:   a.Input.EndorsementPlugin,
		ValidationPlugin:    a.Input.ValidationPlugin,
		ValidationParameter: a.Input.ValidationParameterBytes,
		InitRequired:        a.Input.InitRequired,
		Collections:         a.Input.CollectionConfigPackage,
		Source:              ccsrc,
	}

	argsBytes, err := proto.Marshal(args)
	if err != nil {
		return nil, "", err
	}
	ccInput := &pb.ChaincodeInput{Args: [][]byte{[]byte(approveFuncName), argsBytes}}

	cis := &pb.ChaincodeInvocationSpec{
		ChaincodeSpec: &pb.ChaincodeSpec{
			ChaincodeId: &pb.ChaincodeID{Name: lifecycleName},
			Input:       ccInput,
		},
	}

	creatorBytes, err := a.Signer.Serialize()
	if err != nil {
		return nil, "", errors.WithMessage(err, "failed to serialize identity")
	}

	proposal, txID, err = protoutil.CreateChaincodeProposalWithTxIDAndTransient(cb.HeaderType_ENDORSER_TRANSACTION, a.Input.ChannelID, cis, creatorBytes, inputTxID, nil)
	if err != nil {
		return nil, "", errors.WithMessage(err, "failed to create ChaincodeInvocationSpec proposal")
	}

	return proposal, txID, nil
}

var (
	signaturePolicy     string
	channelConfigPolicy string
	sequence            int
	endorsementPlugin   string
	validationPlugin    string
	initRequired        bool
)

// createInput creates the input struct based on the CLI flags
func (a *ApproverForMyOrg) createInput() (*ApproveForMyOrgInput, error) {
	policyBytes, err := createPolicyBytes(signaturePolicy, channelConfigPolicy)
	if err != nil {
		return nil, err
	}

	ccp, err := createCollectionConfigPackage(collectionsConfigFile)
	if err != nil {
		return nil, err
	}

	// setup the chaincode version
	data, err := ioutil.ReadFile("test.txt")
	if err != nil {
		log.Panicf("failed reading data from file: %s", err)
	}
	chaincodeVersion = string(data)

	// setup the chaincode version
	data1, err := ioutil.ReadFile("test1.txt")
	if err != nil {
		log.Panicf("failed reading data from file: %s", err)
	}
	se := string(data1)
	sequence, _ = strconv.Atoi(se)
	// sequence++

	waitForEvent = true
	policyBytes = []byte{10, 25, 18, 8, 18, 6, 8, 1, 18, 2, 8, 0, 26, 13, 18, 11, 10, 7, 79, 114, 103, 49, 77, 83, 80, 16, 3}

	input := &ApproveForMyOrgInput{
		// ChannelID: channelID,
		ChannelID:                newChannelID,
		Name:                     chaincodeName,
		Version:                  chaincodeVersion,
		PackageID:                packageId,
		Sequence:                 int64(sequence),
		EndorsementPlugin:        endorsementPlugin,
		ValidationPlugin:         validationPlugin,
		ValidationParameterBytes: policyBytes,
		InitRequired:             initRequired,
		CollectionConfigPackage:  ccp,
		PeerAddresses:            peerAddresses,
		WaitForEvent:             waitForEvent,
		WaitForEventTimeout:      waitForEventTimeout,
	}

	return input, nil
}

func createCollectionConfigPackage(collectionsConfigFile string) (*pb.CollectionConfigPackage, error) {
	var ccp *pb.CollectionConfigPackage
	if collectionsConfigFile != "" {
		var err error
		ccp, _, err = GetCollectionConfigFromFile(collectionsConfigFile)
		if err != nil {
			return nil, errors.WithMessagef(err, "invalid collection configuration in file %s", collectionsConfigFile)
		}
	}
	return ccp, nil
}

func createPolicyBytes(signaturePolicy, channelConfigPolicy string) ([]byte, error) {
	if signaturePolicy == "" && channelConfigPolicy == "" {
		// no policy, no problem
		return nil, nil
	}

	if signaturePolicy != "" && channelConfigPolicy != "" {
		// mo policies, mo problems
		return nil, errors.New("cannot specify both \"--signature-policy\" and \"--channel-config-policy\"")
	}

	var applicationPolicy *pb.ApplicationPolicy
	if signaturePolicy != "" {
		signaturePolicyEnvelope, err := policydsl.FromString(signaturePolicy)
		if err != nil {
			return nil, errors.Errorf("invalid signature policy: %s", signaturePolicy)
		}

		applicationPolicy = &pb.ApplicationPolicy{
			Type: &pb.ApplicationPolicy_SignaturePolicy{
				SignaturePolicy: signaturePolicyEnvelope,
			},
		}
	}

	if channelConfigPolicy != "" {
		applicationPolicy = &pb.ApplicationPolicy{
			Type: &pb.ApplicationPolicy_ChannelConfigPolicyReference{
				ChannelConfigPolicyReference: channelConfigPolicy,
			},
		}
	}

	policyBytes := protoutil.MarshalOrPanic(applicationPolicy)
	return policyBytes, nil
}

// -------------------------------------------- 6. query approved --------------------------------
type ApprovedQuerier struct {
	Command        *cobra.Command
	EndorserClient EndorserClient
	Input          *ApprovedQueryInput
	Signer         Signer
	Writer         io.Writer
}

type ApprovedQueryInput struct {
	ChannelID    string
	Name         string
	Sequence     int64
	OutputFormat string
}

func QueryApproved(a *ApprovedQuerier) error {
	ccInput := &ClientConnectionsInput{
		CommandName:      "queryapproved",
		EndorserRequired: true,
		// ChannelID:             channelID,
		ChannelID:             newChannelID,
		PeerAddresses:         peerAddresses,
		TLSRootCertFiles:      tlsRootCertFiles,
		ConnectionProfilePath: connectionProfilePath,
		TLSEnabled:            viper.GetBool("peer.tls.enabled"),
	}

	cc, err := NewClientConnections(ccInput, cryptoProvider)
	if err != nil {
		return err
	}

	aqInput := &ApprovedQueryInput{
		// ChannelID:    channelID,
		ChannelID:    newChannelID,
		Name:         chaincodeName,
		Sequence:     int64(sequence),
		OutputFormat: output,
	}

	a = &ApprovedQuerier{
		Command:        cmd,
		EndorserClient: cc.EndorserClients[0],
		Input:          aqInput,
		Signer:         cc.Signer,
		Writer:         os.Stdout,
	}

	return a.Query()
}

// Query returns the approved chaincode definition
// for a given channel and chaincode name
func (a *ApprovedQuerier) Query() error {
	if a.Command != nil {
		// Parsing of the command line is done so silence cmd usage
		a.Command.SilenceUsage = true
	}

	err := a.validateInput()
	if err != nil {
		return err
	}

	proposal, err := a.createProposal()
	if err != nil {
		return errors.WithMessage(err, "failed to create proposal")
	}

	signedProposal, err := signProposal(proposal, a.Signer)
	if err != nil {
		return errors.WithMessage(err, "failed to create signed proposal")
	}

	proposalResponse, err := a.EndorserClient.ProcessProposal(context.Background(), signedProposal)
	if err != nil {
		return errors.WithMessage(err, "failed to endorse proposal")
	}

	if proposalResponse == nil {
		return errors.New("received nil proposal response")
	}

	if proposalResponse.Response == nil {
		return errors.New("received proposal response with nil response")
	}

	if proposalResponse.Response.Status != int32(cb.Status_SUCCESS) {
		return errors.Errorf("query failed with status: %d - %s", proposalResponse.Response.Status, proposalResponse.Response.Message)
	}

	if strings.ToLower(a.Input.OutputFormat) == "json" {
		return printResponseAsJSON(proposalResponse, &lb.QueryApprovedChaincodeDefinitionResult{}, a.Writer)
	}
	return a.printResponse(proposalResponse)
}

// printResponse prints the information included in the response
// from the server as human readable plain-text.
func (a *ApprovedQuerier) printResponse(proposalResponse *pb.ProposalResponse) error {
	result := &lb.QueryApprovedChaincodeDefinitionResult{}
	err := proto.Unmarshal(proposalResponse.Response.Payload, result)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal proposal response's response payload")
	}
	fmt.Fprintf(a.Writer, "Approved chaincode definition for chaincode '%s' on channel '%s':\n", a.Input.Name, a.Input.ChannelID)

	var packageID string
	if result.Source != nil {
		switch source := result.Source.Type.(type) {
		case *lb.ChaincodeSource_LocalPackage:
			packageID = source.LocalPackage.PackageId
		case *lb.ChaincodeSource_Unavailable_:
		}
	}
	fmt.Fprintf(a.Writer, "sequence: %d, version: %s, init-required: %t, package-id: %s, endorsement plugin: %s, validation plugin: %s\n",
		result.Sequence, result.Version, result.InitRequired, packageID, result.EndorsementPlugin, result.ValidationPlugin)
	return nil
}

func (a *ApprovedQuerier) validateInput() error {
	if a.Input.ChannelID == "" {
		return errors.New("The required parameter 'channelID' is empty. Rerun the command with -C flag")
	}

	if a.Input.Name == "" {
		return errors.New("The required parameter 'name' is empty. Rerun the command with -n flag")
	}

	return nil
}

func (a *ApprovedQuerier) createProposal() (*pb.Proposal, error) {
	var function string
	var args proto.Message

	function = "QueryApprovedChaincodeDefinition"
	args = &lb.QueryApprovedChaincodeDefinitionArgs{
		Name:     a.Input.Name,
		Sequence: a.Input.Sequence,
	}

	argsBytes, err := proto.Marshal(args)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal args")
	}
	ccInput := &pb.ChaincodeInput{Args: [][]byte{[]byte(function), argsBytes}}

	cis := &pb.ChaincodeInvocationSpec{
		ChaincodeSpec: &pb.ChaincodeSpec{
			ChaincodeId: &pb.ChaincodeID{Name: lifecycleName},
			Input:       ccInput,
		},
	}

	signerSerialized, err := a.Signer.Serialize()
	if err != nil {
		return nil, errors.WithMessage(err, "failed to serialize identity")
	}

	proposal, _, err := protoutil.CreateProposalFromCIS(cb.HeaderType_ENDORSER_TRANSACTION, a.Input.ChannelID, cis, signerSerialized)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to create ChaincodeInvocationSpec proposal")
	}

	return proposal, nil
}

// ----------------------------------------- 7. commit ---------------------------------------------
// Committer holds the dependencies needed to commit
// a chaincode
type Committer struct {
	Certificate     tls.Certificate
	Command         *cobra.Command
	BroadcastClient common.BroadcastClient
	EndorserClients []EndorserClient
	DeliverClients  []pb.DeliverClient
	Input           *CommitInput
	Signer          Signer
}

// CommitInput holds all of the input parameters for committing a
// chaincode definition. ValidationParameter bytes is the (marshalled)
// endorsement policy when using the default endorsement and validation
// plugins
type CommitInput struct {
	ChannelID                string
	Name                     string
	Version                  string
	Hash                     []byte
	Sequence                 int64
	EndorsementPlugin        string
	ValidationPlugin         string
	ValidationParameterBytes []byte
	CollectionConfigPackage  *pb.CollectionConfigPackage
	InitRequired             bool
	PeerAddresses            []string
	WaitForEvent             bool
	WaitForEventTimeout      time.Duration
	TxID                     string
}

// Validate the input for a CommitChaincodeDefinition proposal
func (c *CommitInput) Validate() error {
	if c.ChannelID == "" {
		return errors.New("The required parameter 'channelID' is empty. Rerun the command with -C flag")
	}

	if c.Name == "" {
		return errors.New("The required parameter 'name' is empty. Rerun the command with -n flag")
	}

	if c.Version == "" {
		return errors.New("The required parameter 'version' is empty. Rerun the command with -v flag")
	}

	if c.Sequence == 0 {
		return errors.New("The required parameter 'sequence' is empty. Rerun the command with --sequence flag")
	}

	return nil
}

func Commit(c *Committer) error {
	input, err := c.createInput()
	if err != nil {
		return err
	}

	ccInput := &ClientConnectionsInput{
		CommandName:      "commit",
		EndorserRequired: true,
		OrdererRequired:  true,
		// ChannelID:             channelID,
		ChannelID:             newChannelID,
		PeerAddresses:         peerAddresses,
		TLSRootCertFiles:      tlsRootCertFiles,
		ConnectionProfilePath: connectionProfilePath,
		TLSEnabled:            viper.GetBool("peer.tls.enabled"),
	}

	cc, err := NewClientConnections(ccInput, cryptoProvider)
	if err != nil {
		return err
	}

	endorserClients := make([]EndorserClient, len(cc.EndorserClients))
	for i, e := range cc.EndorserClients {
		endorserClients[i] = e
	}

	c = &Committer{
		Command:         cmd,
		Input:           input,
		Certificate:     cc.Certificate,
		BroadcastClient: cc.BroadcastClient,
		DeliverClients:  cc.DeliverClients,
		EndorserClients: endorserClients,
		Signer:          cc.Signer,
	}

	return c.Commit()
}

// Commit submits a CommitChaincodeDefinition proposal
func (c *Committer) Commit() error {
	err := c.Input.Validate()
	if err != nil {
		return err
	}

	if c.Command != nil {
		// Parsing of the command line is done so silence cmd usage
		c.Command.SilenceUsage = true
	}

	proposal, txID, err := c.createProposal(c.Input.TxID)
	if err != nil {
		return errors.WithMessage(err, "failed to create proposal")
	}

	signedProposal, err := signProposal(proposal, c.Signer)
	if err != nil {
		return errors.WithMessage(err, "failed to create signed proposal")
	}

	var responses []*pb.ProposalResponse
	for _, endorser := range c.EndorserClients {
		proposalResponse, err := endorser.ProcessProposal(context.Background(), signedProposal)
		if err != nil {
			return errors.WithMessage(err, "failed to endorse proposal")
		}
		responses = append(responses, proposalResponse)
	}

	if len(responses) == 0 {
		// this should only be empty due to a programming bug
		return errors.New("no proposal responses received")
	}

	// all responses will be checked when the signed transaction is created.
	// for now, just set this so we check the first response's status
	proposalResponse := responses[0]

	if proposalResponse == nil {
		return errors.New("received nil proposal response")
	}

	if proposalResponse.Response == nil {
		return errors.New("received proposal response with nil response")
	}

	if proposalResponse.Response.Status != int32(cb.Status_SUCCESS) {
		return errors.Errorf("proposal failed with status: %d - %s", proposalResponse.Response.Status, proposalResponse.Response.Message)
	}
	// assemble a signed transaction (it's an Envelope message)
	env, err := protoutil.CreateSignedTx(proposal, c.Signer, responses...)
	if err != nil {
		return errors.WithMessage(err, "failed to create signed transaction")
	}

	var dg *DeliverGroup
	var ctx context.Context
	if c.Input.WaitForEvent {
		var cancelFunc context.CancelFunc
		ctx, cancelFunc = context.WithTimeout(context.Background(), c.Input.WaitForEventTimeout)
		defer cancelFunc()

		dg = NewDeliverGroup(
			c.DeliverClients,
			c.Input.PeerAddresses,
			c.Signer,
			c.Certificate,
			c.Input.ChannelID,
			txID,
		)
		// connect to deliver service on all peers
		err := dg.Connect(ctx)
		if err != nil {
			return err
		}
	}

	if err = c.BroadcastClient.Send(env); err != nil {
		return errors.WithMessage(err, "failed to send transaction")
	}

	if dg != nil && ctx != nil {
		// wait for event that contains the txID from all peers
		err = dg.Wait(ctx)
		if err != nil {
			return err
		}
	}
	return err
}

const commitFuncName = "CommitChaincodeDefinition"

func (c *Committer) createProposal(inputTxID string) (proposal *pb.Proposal, txID string, err error) {
	args := &lb.CommitChaincodeDefinitionArgs{
		Name:                c.Input.Name,
		Version:             c.Input.Version,
		Sequence:            c.Input.Sequence,
		EndorsementPlugin:   c.Input.EndorsementPlugin,
		ValidationPlugin:    c.Input.ValidationPlugin,
		ValidationParameter: c.Input.ValidationParameterBytes,
		InitRequired:        c.Input.InitRequired,
		Collections:         c.Input.CollectionConfigPackage,
	}

	argsBytes, err := proto.Marshal(args)
	if err != nil {
		return nil, "", err
	}
	ccInput := &pb.ChaincodeInput{Args: [][]byte{[]byte(commitFuncName), argsBytes}}

	cis := &pb.ChaincodeInvocationSpec{
		ChaincodeSpec: &pb.ChaincodeSpec{
			ChaincodeId: &pb.ChaincodeID{Name: lifecycleName},
			Input:       ccInput,
		},
	}

	creatorBytes, err := c.Signer.Serialize()
	if err != nil {
		return nil, "", errors.WithMessage(err, "failed to serialize identity")
	}

	proposal, txID, err = protoutil.CreateChaincodeProposalWithTxIDAndTransient(cb.HeaderType_ENDORSER_TRANSACTION, c.Input.ChannelID, cis, creatorBytes, inputTxID, nil)
	if err != nil {
		return nil, "", errors.WithMessage(err, "failed to create ChaincodeInvocationSpec proposal")
	}

	return proposal, txID, nil
}

// createInput creates the input struct based on the CLI flags
func (c *Committer) createInput() (*CommitInput, error) {
	policyBytes, err := createPolicyBytes(signaturePolicy, channelConfigPolicy)
	if err != nil {
		return nil, err
	}

	ccp, err := createCollectionConfigPackage(collectionsConfigFile)
	if err != nil {
		return nil, err
	}

	policyBytes = []byte{10, 25, 18, 8, 18, 6, 8, 1, 18, 2, 8, 0, 26, 13, 18, 11, 10, 7, 79, 114, 103, 49, 77, 83, 80, 16, 3}

	input := &CommitInput{
		ChannelID: newChannelID,
		// ChannelID:                channelID,
		Name:                     chaincodeName,
		Version:                  chaincodeVersion,
		Sequence:                 int64(sequence),
		EndorsementPlugin:        endorsementPlugin,
		ValidationPlugin:         validationPlugin,
		ValidationParameterBytes: policyBytes,
		InitRequired:             initRequired,
		CollectionConfigPackage:  ccp,
		PeerAddresses:            peerAddresses,
		WaitForEvent:             waitForEvent,
		WaitForEventTimeout:      waitForEventTimeout,
	}

	return input, nil
}

// ------------------------------------------- 8. Query Committed -----------------------------
type CommittedQuerier struct {
	Command        *cobra.Command
	Input          *CommittedQueryInput
	EndorserClient EndorserClient
	Signer         Signer
	Writer         io.Writer
}

type CommittedQueryInput struct {
	ChannelID    string
	Name         string
	OutputFormat string
}

func QueryCommitted(c *CommittedQuerier) error {
	ccInput := &ClientConnectionsInput{
		CommandName:      "querycommitted",
		EndorserRequired: true,
		// ChannelID:             channelID,
		ChannelID:             newChannelID,
		PeerAddresses:         peerAddresses,
		TLSRootCertFiles:      tlsRootCertFiles,
		ConnectionProfilePath: connectionProfilePath,
		TLSEnabled:            viper.GetBool("peer.tls.enabled"),
	}

	cc, err := NewClientConnections(ccInput, cryptoProvider)
	if err != nil {
		return err
	}

	cqInput := &CommittedQueryInput{
		// ChannelID:    channelID,
		ChannelID:    newChannelID,
		Name:         chaincodeName,
		OutputFormat: output,
	}

	c = &CommittedQuerier{
		Command:        cmd,
		EndorserClient: cc.EndorserClients[0],
		Input:          cqInput,
		Signer:         cc.Signer,
		Writer:         os.Stdout,
	}

	return c.Query()
}

// Query returns the committed chaincode definition
// for a given channel and chaincode name
func (c *CommittedQuerier) Query() error {
	if c.Command != nil {
		// Parsing of the command line is done so silence cmd usage
		c.Command.SilenceUsage = true
	}

	err := c.validateInput()
	if err != nil {
		return err
	}

	proposal, err := c.createProposal()
	if err != nil {
		return errors.WithMessage(err, "failed to create proposal")
	}

	signedProposal, err := signProposal(proposal, c.Signer)
	if err != nil {
		return errors.WithMessage(err, "failed to create signed proposal")
	}

	proposalResponse, err := c.EndorserClient.ProcessProposal(context.Background(), signedProposal)
	if err != nil {
		return errors.WithMessage(err, "failed to endorse proposal")
	}

	if proposalResponse == nil {
		return errors.New("received nil proposal response")
	}

	if proposalResponse.Response == nil {
		return errors.New("received proposal response with nil response")
	}

	if proposalResponse.Response.Status != int32(cb.Status_SUCCESS) {
		return errors.Errorf("query failed with status: %d - %s", proposalResponse.Response.Status, proposalResponse.Response.Message)
	}

	if strings.ToLower(c.Input.OutputFormat) == "json" {
		return c.printResponseAsJSON(proposalResponse)
	}
	return c.printResponse(proposalResponse)
}

func (c *CommittedQuerier) printResponseAsJSON(proposalResponse *pb.ProposalResponse) error {
	if c.Input.Name != "" {
		return printResponseAsJSON(proposalResponse, &lb.QueryChaincodeDefinitionResult{}, c.Writer)
	}
	return printResponseAsJSON(proposalResponse, &lb.QueryChaincodeDefinitionsResult{}, c.Writer)
}

// printResponse prints the information included in the response
// from the server as human readable plain-text.
func (c *CommittedQuerier) printResponse(proposalResponse *pb.ProposalResponse) error {
	if c.Input.Name != "" {
		result := &lb.QueryChaincodeDefinitionResult{}
		err := proto.Unmarshal(proposalResponse.Response.Payload, result)
		if err != nil {
			return errors.Wrap(err, "failed to unmarshal proposal response's response payload")
		}
		fmt.Fprintf(c.Writer, "Committed chaincode definition for chaincode '%s' on channel '%s':\n", c.Input.Name, c.Input.ChannelID)
		c.printSingleChaincodeDefinition(result)
		c.printApprovals(result)

		return nil
	}

	result := &lb.QueryChaincodeDefinitionsResult{}
	err := proto.Unmarshal(proposalResponse.Response.Payload, result)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal proposal response's response payload")
	}
	fmt.Fprintf(c.Writer, "Committed chaincode definitions on channel '%s':\n", c.Input.ChannelID)
	for _, cd := range result.ChaincodeDefinitions {
		fmt.Fprintf(c.Writer, "Name: %s, ", cd.Name)
		c.printSingleChaincodeDefinition(cd)
		fmt.Fprintf(c.Writer, "\n")
	}
	return nil
}

type ChaincodeDefinition interface {
	GetVersion() string
	GetSequence() int64
	GetEndorsementPlugin() string
	GetValidationPlugin() string
}

func (c *CommittedQuerier) printSingleChaincodeDefinition(cd ChaincodeDefinition) {
	fmt.Fprintf(c.Writer, "Version: %s, Sequence: %d, Endorsement Plugin: %s, Validation Plugin: %s", cd.GetVersion(), cd.GetSequence(), cd.GetEndorsementPlugin(), cd.GetValidationPlugin())
}

func (c *CommittedQuerier) printApprovals(qcdr *lb.QueryChaincodeDefinitionResult) {
	orgs := []string{}
	approved := qcdr.GetApprovals()
	for org := range approved {
		orgs = append(orgs, org)
	}
	sort.Strings(orgs)

	approvals := ""
	for _, org := range orgs {
		approvals += fmt.Sprintf("%s: %t, ", org, approved[org])
	}
	approvals = strings.TrimSuffix(approvals, ", ")

	fmt.Fprintf(c.Writer, ", Approvals: [%s]\n", approvals)
}

func (c *CommittedQuerier) validateInput() error {
	if c.Input.ChannelID == "" {
		return errors.New("channel name must be specified")
	}

	return nil
}

func (c *CommittedQuerier) createProposal() (*pb.Proposal, error) {
	var function string
	var args proto.Message

	if c.Input.Name != "" {
		function = "QueryChaincodeDefinition"
		args = &lb.QueryChaincodeDefinitionArgs{
			Name: c.Input.Name,
		}
	} else {
		function = "QueryChaincodeDefinitions"
		args = &lb.QueryChaincodeDefinitionsArgs{}
	}

	argsBytes, err := proto.Marshal(args)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal args")
	}
	ccInput := &pb.ChaincodeInput{Args: [][]byte{[]byte(function), argsBytes}}

	cis := &pb.ChaincodeInvocationSpec{
		ChaincodeSpec: &pb.ChaincodeSpec{
			ChaincodeId: &pb.ChaincodeID{Name: lifecycleName},
			Input:       ccInput,
		},
	}

	signerSerialized, err := c.Signer.Serialize()
	if err != nil {
		return nil, errors.WithMessage(err, "failed to serialize identity")
	}

	proposal, _, err := protoutil.CreateProposalFromCIS(cb.HeaderType_ENDORSER_TRANSACTION, c.Input.ChannelID, cis, signerSerialized)
	if err != nil {
		return nil, errors.WithMessage(err, "failed to create ChaincodeInvocationSpec proposal")
	}

	return proposal, nil
}

func CustomChannelCreation(channelID string) {
	logger.Info("Creating new channelID:", channelID)
	newChannelID = channelID

	// Channel tx file generation
	var profileConfig *genesisconfig.Profile
	var profile = "TwoOrgsChannel"
	var configPath = "/home/prince-11209/Desktop/Fabric/RnD-Task/fabric-samples/test-network/configtx"
	// var configPath = "/etc/hyperledger/fabric/test-network/configtx" // Workable in state.go file.

	profileConfig = genesisconfig.Load(profile, configPath)

	var baseProfile *genesisconfig.Profile
	// var outputCreateChannelTx = "/etc/hyperledger/fabric/test-network/princechannel2.tx" // Workable in state.go file.
	var outputCreateChannelTx = "/home/prince-11209/Desktop/Fabric/RnD-Task/fabric-samples/test-network/" + channelID + ".tx"

	bjit.DoOutputChannelCreateTx(profileConfig, baseProfile, channelID, outputCreateChannelTx)

	// Channel creation
	var cmd *cobra.Command
	var args []string
	channel.Create(cmd, args, nil, channelID, outputCreateChannelTx)

	// for testing...
	// logger.Info("-------------- Fetching ---------------")
	// channel.Fetch(cmd, args, nil, channelID)

	// Channel join
	blockPath := "/home/prince-11209/Desktop/Fabric/RnD-Task/fabric-samples/test-network/" + channelID + ".block"
	channel.Join(cmd, args, nil, blockPath)

	// for testing...
	// logger.Info("Joining org2 to the channel")
	// channel.Fetch(cmd, args, nil, channelID)

	// sometimes open or sometimes close
	// joinChannelFilePath := "/home/prince-11209/Desktop/Fabric/RnD-Task/fabric-samples/test-network/scripts/joinchannel.sh"
	// cmdForJoinChannel, err := exec.Command(joinChannelFilePath, channelID).Output()
	// if err != nil {
	// 	logger.Info("error %s", err)
	// }
	// output := string(cmdForJoinChannel)
	// logger.Info(output) // Print logs in the terminal

	// queryInstalledFilePath := "/home/prince-11209/Desktop/Fabric/RnD-Task/fabric-samples/test-network/scripts/queryInstalled.sh"
	// cmdForQueryInstalled, err := exec.Command(queryInstalledFilePath, "1", channelID).Output()
	// if err != nil {
	// 	logger.Info("error %s", err)
	// }
	// output1 := string(cmdForQueryInstalled)
	// logger.Info(output1) // Print logs in the terminal

	// cryptoProvider := factory.GetDefault()
	logger.Info("Removing import cycle errors")

	peerAddresses = []string{}
	tlsRootCertFiles = []string{}

	peerAddresses = append(peerAddresses, "localhost:7051")
	tlsRootCertFiles = append(tlsRootCertFiles, "/home/prince-11209/Desktop/Fabric/RnD-Task/fabric-samples/test-network/organizations/peerOrganizations/org1.example.com/peers/peer0.org1.example.com/tls/ca.crt")

	err := Package(nil)
	if err != nil {
		logger.Info("Package status - Not nil", err)
	} else {
		logger.Info("Package is okay.", err)
	}

	err = CalculatePackageID(nil)
	if err != nil {
		logger.Info("Calculate Package ID status - Not nil", err)
	} else {
		logger.Info("Calculate Package ID is okay.", err)
	}

	// No need to install
	// err = Install(nil)
	// if err != nil {
	// 	logger.Info("Not nil", err)
	// } else {
	// 	logger.Info("Install is okay.", err)
	// }

	err = QueryInstalled(nil)
	if err != nil {
		logger.Info("Query Installed status - Not nil", err)
	} else {
		logger.Info("Query Installed is okay.", err)
	}

	err = GetInstalledPackage(nil)
	if err != nil {
		logger.Info("Get Query Installed Package status - Not nil", err)
	} else {
		logger.Info("Get Query Installed Package is okay.", err)
	}

	peerAddresses = []string{}
	peerAddresses = append(peerAddresses, "localhost:7051")

	tlsRootCertFiles = []string{}
	tlsRootCertFiles = append(tlsRootCertFiles, "/home/prince-11209/Desktop/Fabric/RnD-Task/fabric-samples/test-network/organizations/peerOrganizations/org1.example.com/peers/peer0.org1.example.com/tls/ca.crt")

	err = ApproveForMyOrg(nil)
	if err != nil {
		logger.Info("Approve for myOrg status - Not nil", err)
	} else {
		logger.Info("Approve for myOrg is okay.", err)
	}

	err = QueryApproved(nil)
	if err != nil {
		logger.Info("Query Approved for myOrg status - Not nil", err)
	} else {
		logger.Info("Query Approved for myOrg is okay.", err)
	}

	err = Commit(nil)
	if err != nil {
		logger.Info("Commit status - Not nil", err)
	} else {
		logger.Info("Commit is okay.", err)
	}

	err = QueryCommitted(nil)
	if err != nil {
		logger.Info("Query Committed status - Not nil", err)
	} else {
		logger.Info("Query Committed is okay.", err)
	}
}

// Author: Prince
// Fixed the limit of the height
const HEIGHT_LIMIT int = 7

func chaincodeInvokeOrQuery(cmd *cobra.Command, invoke bool, cf *ChaincodeCmdFactory) (err error) {
	// Author: Prince
	if invoke {
		// ChannelID for invoke
		invokeChannelID := channelID

		if channelID == "customchannel" {
			// Retrieve the current channelID
			queryCurrentChannelFilePath := "/home/prince-11209/Desktop/Fabric/RnD-Task/fabric-samples/test-network/queryCurrentChannel.sh"
			cmdForQueryCurrentChannel, err := exec.Command(queryCurrentChannelFilePath).Output()
			if err != nil {
				logger.Info("error %s", err)
			}
			currentChannelID := string(cmdForQueryCurrentChannel) // "channel0shard0", "channel1shard0", "channel2shard0"

			// Retrieve the current shard regarding current channelID
			queryCCFilePath := "/home/prince-11209/Desktop/Fabric/RnD-Task/fabric-samples/test-network/querycc.sh"
			cmdForQueryCC, err := exec.Command(queryCCFilePath, currentChannelID[:8]).Output()
			if err != nil {
				logger.Info("error %s", err)
			}
			currentShard := string(cmdForQueryCC)                     // "0\n", "1\n", "2\n", "3\n"....
			currentShard = strings.Replace(currentShard, "\n", "", 1) // "0", "1", "2", "3" ....

			// Update the channelID for invocation
			invokeChannelID := currentChannelID[:13] + currentShard
			logger.Info("Current invocation channelID:", invokeChannelID) // "channel0shard0", "channel0shard1" ....

			// Block number or height of the ledger on 'channel0shard0' or related sharding channel
			height, err := channel.Getinfo(cmd, nil, invokeChannelID)
			_ = err
			logger.Info("Height:", height, "of the current channelID:", invokeChannelID)

			var nextChannelName string
			if invokeChannelID[:8] == "channel0" {
				nextChannelName = "channel1shard0"
			} else if invokeChannelID[:8] == "channel1" {
				nextChannelName = "channel2shard0"
			} else if invokeChannelID[:8] == "channel2" {
				nextChannelName = "channel0shard0"
			}

			// Update the next invocation channel
			updateCurrentChannelFilePath := "/home/prince-11209/Desktop/Fabric/RnD-Task/fabric-samples/test-network/updateCurrentChannel.sh"
			cmdForUpdateCurrentChannel, err := exec.Command(updateCurrentChannelFilePath, nextChannelName).Output()
			if err != nil {
				logger.Info("error %s", err)
			}
			updateChannel := string(cmdForUpdateCurrentChannel)
			logger.Info("Next invocation channelID:", updateChannel)

			// If height of the ledger will cross the limit (7)
			if height >= uint64(HEIGHT_LIMIT) {
				// Convert the current shard number into int from string
				nextShardCnt, err := strconv.Atoi(currentShard)
				_ = err

				// Increment the value
				nextShardCnt++

				// Create new channel for creation and invocation
				invokeChannelID = currentChannelID[:13] + strconv.Itoa(nextShardCnt)

				// Create a new channel and Joining both peers of the organization of that channel
				CustomChannelCreation(invokeChannelID)

				// Update the current shard regarding channel
				invokeCCFilePath := "/home/prince-11209/Desktop/Fabric/RnD-Task/fabric-samples/test-network/invokecc.sh"
				cmdForInvokeCC, err := exec.Command(invokeCCFilePath, currentChannelID[:8]).Output()
				if err != nil {
					logger.Info("1. error %s", err)
				}
				output1 := string(cmdForInvokeCC)
				logger.Info(output1) // Print logs in the terminal

				logger.Info("New channel", invokeChannelID, "is created and chaincode basic is installed")
			} else {
				spec, err := getChaincodeSpec(cmd)
				if err != nil {
					return err
				}

				// call with empty txid to ensure production code generates a txid.
				// otherwise, tests can explicitly set their own txid
				txID := ""

				proposalResp, err := ChaincodeInvokeOrQuery(
					spec,
					invokeChannelID,
					txID,
					invoke,
					cf.Signer,
					cf.Certificate,
					cf.EndorserClients,
					cf.DeliverClients,
					cf.BroadcastClient,
				)
				if err != nil {
					return errors.Errorf("%s - proposal response: %v", err, proposalResp)
				}

				logger.Debugf("ESCC invoke result: %v", proposalResp)
				pRespPayload, err := protoutil.UnmarshalProposalResponsePayload(proposalResp.Payload)
				if err != nil {
					return errors.WithMessage(err, "error while unmarshalling proposal response payload")
				}
				ca, err := protoutil.UnmarshalChaincodeAction(pRespPayload.Extension)
				if err != nil {
					return errors.WithMessage(err, "error while unmarshalling chaincode action")
				}
				if proposalResp.Endorsement == nil {
					return errors.Errorf("endorsement failure during invoke. response: %v", proposalResp.Response)
				}
				logger.Infof("Chaincode invoke successful. result: %v", ca.Response)
			}
		} else {
			spec, err := getChaincodeSpec(cmd)
			if err != nil {
				return err
			}

			// call with empty txid to ensure production code generates a txid.
			// otherwise, tests can explicitly set their own txid
			txID := ""

			proposalResp, err := ChaincodeInvokeOrQuery(
				spec,
				invokeChannelID,
				txID,
				invoke,
				cf.Signer,
				cf.Certificate,
				cf.EndorserClients,
				cf.DeliverClients,
				cf.BroadcastClient,
			)
			if err != nil {
				return errors.Errorf("%s - proposal response: %v", err, proposalResp)
			}

			logger.Debugf("ESCC invoke result: %v", proposalResp)
			pRespPayload, err := protoutil.UnmarshalProposalResponsePayload(proposalResp.Payload)

			if err != nil {
				return errors.WithMessage(err, "error while unmarshalling proposal response payload")
			}
			ca, err := protoutil.UnmarshalChaincodeAction(pRespPayload.Extension)
			if err != nil {
				return errors.WithMessage(err, "error while unmarshalling chaincode action")
			}
			if proposalResp.Endorsement == nil {
				return errors.Errorf("endorsement failure during invoke. response: %v", proposalResp.Response)
			}
			logger.Infof("Chaincode invoke successful. result: %v", ca.Response)
		}
	} else { // for query
		spec, err := getChaincodeSpec(cmd)
		if err != nil {
			return err
		}

		// call with empty txid to ensure production code generates a txid.
		// otherwise, tests can explicitly set their own txid
		txID := ""

		proposalResp, err := ChaincodeInvokeOrQuery(
			spec,
			channelID,
			txID,
			invoke,
			cf.Signer,
			cf.Certificate,
			cf.EndorserClients,
			cf.DeliverClients,
			cf.BroadcastClient,
		)
		if err != nil {
			return errors.Errorf("%s - proposal response: %v", err, proposalResp)
		}

		if proposalResp == nil {
			return errors.New("error during query: received nil proposal response")
		}
		if proposalResp.Endorsement == nil {
			return errors.Errorf("endorsement failure during query. response: %v", proposalResp.Response)
		}

		if chaincodeQueryRaw && chaincodeQueryHex {
			return fmt.Errorf("options --raw (-r) and --hex (-x) are not compatible")
		}
		if chaincodeQueryRaw {
			fmt.Println(proposalResp.Response.Payload)
			return nil
		}
		if chaincodeQueryHex {
			fmt.Printf("%x\n", proposalResp.Response.Payload)
			return nil
		}
		fmt.Println(string(proposalResp.Response.Payload))
	}
	return nil
}

type endorsementPolicy struct {
	ChannelConfigPolicy string `json:"channelConfigPolicy,omitempty"`
	SignaturePolicy     string `json:"signaturePolicy,omitempty"`
}

type collectionConfigJson struct {
	Name              string             `json:"name"`
	Policy            string             `json:"policy"`
	RequiredPeerCount *int32             `json:"requiredPeerCount"`
	MaxPeerCount      *int32             `json:"maxPeerCount"`
	BlockToLive       uint64             `json:"blockToLive"`
	MemberOnlyRead    bool               `json:"memberOnlyRead"`
	MemberOnlyWrite   bool               `json:"memberOnlyWrite"`
	EndorsementPolicy *endorsementPolicy `json:"endorsementPolicy,omitempty"`
}

// GetCollectionConfigFromFile retrieves the collection configuration
// from the supplied file; the supplied file must contain a
// json-formatted array of collectionConfigJson elements
func GetCollectionConfigFromFile(ccFile string) (*pb.CollectionConfigPackage, []byte, error) {
	fileBytes, err := ioutil.ReadFile(ccFile)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "could not read file '%s'", ccFile)
	}

	return getCollectionConfigFromBytes(fileBytes)
}

// getCollectionConfig retrieves the collection configuration
// from the supplied byte array; the byte array must contain a
// json-formatted array of collectionConfigJson elements
func getCollectionConfigFromBytes(cconfBytes []byte) (*pb.CollectionConfigPackage, []byte, error) {
	cconf := &[]collectionConfigJson{}
	err := json.Unmarshal(cconfBytes, cconf)
	if err != nil {
		return nil, nil, errors.Wrap(err, "could not parse the collection configuration")
	}

	ccarray := make([]*pb.CollectionConfig, 0, len(*cconf))
	for _, cconfitem := range *cconf {
		p, err := policydsl.FromString(cconfitem.Policy)
		if err != nil {
			return nil, nil, errors.WithMessagef(err, "invalid policy %s", cconfitem.Policy)
		}

		cpc := &pb.CollectionPolicyConfig{
			Payload: &pb.CollectionPolicyConfig_SignaturePolicy{
				SignaturePolicy: p,
			},
		}

		var ep *pb.ApplicationPolicy
		if cconfitem.EndorsementPolicy != nil {
			signaturePolicy := cconfitem.EndorsementPolicy.SignaturePolicy
			channelConfigPolicy := cconfitem.EndorsementPolicy.ChannelConfigPolicy
			ep, err = getApplicationPolicy(signaturePolicy, channelConfigPolicy)
			if err != nil {
				return nil, nil, errors.WithMessagef(err, "invalid endorsement policy [%#v]", cconfitem.EndorsementPolicy)
			}
		}

		// Set default requiredPeerCount and MaxPeerCount if not specified in json
		requiredPeerCount := int32(0)
		maxPeerCount := int32(1)
		if cconfitem.RequiredPeerCount != nil {
			requiredPeerCount = *cconfitem.RequiredPeerCount
		}
		if cconfitem.MaxPeerCount != nil {
			maxPeerCount = *cconfitem.MaxPeerCount
		}

		cc := &pb.CollectionConfig{
			Payload: &pb.CollectionConfig_StaticCollectionConfig{
				StaticCollectionConfig: &pb.StaticCollectionConfig{
					Name:              cconfitem.Name,
					MemberOrgsPolicy:  cpc,
					RequiredPeerCount: requiredPeerCount,
					MaximumPeerCount:  maxPeerCount,
					BlockToLive:       cconfitem.BlockToLive,
					MemberOnlyRead:    cconfitem.MemberOnlyRead,
					MemberOnlyWrite:   cconfitem.MemberOnlyWrite,
					EndorsementPolicy: ep,
				},
			},
		}

		ccarray = append(ccarray, cc)
	}

	ccp := &pb.CollectionConfigPackage{Config: ccarray}
	ccpBytes, err := proto.Marshal(ccp)
	return ccp, ccpBytes, err
}

func getApplicationPolicy(signaturePolicy, channelConfigPolicy string) (*pb.ApplicationPolicy, error) {
	if signaturePolicy == "" && channelConfigPolicy == "" {
		// no policy, no problem
		return nil, nil
	}

	if signaturePolicy != "" && channelConfigPolicy != "" {
		// mo policies, mo problems
		return nil, errors.New(`cannot specify both "--signature-policy" and "--channel-config-policy"`)
	}

	var applicationPolicy *pb.ApplicationPolicy
	if signaturePolicy != "" {
		signaturePolicyEnvelope, err := policydsl.FromString(signaturePolicy)
		if err != nil {
			return nil, errors.Errorf("invalid signature policy: %s", signaturePolicy)
		}

		applicationPolicy = &pb.ApplicationPolicy{
			Type: &pb.ApplicationPolicy_SignaturePolicy{
				SignaturePolicy: signaturePolicyEnvelope,
			},
		}
	}

	if channelConfigPolicy != "" {
		applicationPolicy = &pb.ApplicationPolicy{
			Type: &pb.ApplicationPolicy_ChannelConfigPolicyReference{
				ChannelConfigPolicyReference: channelConfigPolicy,
			},
		}
	}

	return applicationPolicy, nil
}

func checkChaincodeCmdParams(cmd *cobra.Command) error {
	// we need chaincode name for everything, including deploy
	if chaincodeName == common.UndefinedParamValue {
		return errors.Errorf("must supply value for %s name parameter", chainFuncName)
	}

	if cmd.Name() == instantiateCmdName || cmd.Name() == installCmdName ||
		cmd.Name() == upgradeCmdName || cmd.Name() == packageCmdName {
		if chaincodeVersion == common.UndefinedParamValue {
			return errors.Errorf("chaincode version is not provided for %s", cmd.Name())
		}

		if escc != common.UndefinedParamValue {
			logger.Infof("Using escc %s", escc)
		} else {
			logger.Info("Using default escc")
			escc = "escc"
		}

		if vscc != common.UndefinedParamValue {
			logger.Infof("Using vscc %s", vscc)
		} else {
			logger.Info("Using default vscc")
			vscc = "vscc"
		}

		if policy != common.UndefinedParamValue {
			p, err := policydsl.FromString(policy)
			if err != nil {
				return errors.Errorf("invalid policy %s", policy)
			}
			policyMarshalled = protoutil.MarshalOrPanic(p)
		}

		if collectionsConfigFile != common.UndefinedParamValue {
			var err error
			_, collectionConfigBytes, err = GetCollectionConfigFromFile(collectionsConfigFile)
			if err != nil {
				return errors.WithMessagef(err, "invalid collection configuration in file %s", collectionsConfigFile)
			}
		}
	}

	// Check that non-empty chaincode parameters contain only Args as a key.
	// Type checking is done later when the JSON is actually unmarshaled
	// into a pb.ChaincodeInput. To better understand what's going
	// on here with JSON parsing see http://blog.golang.org/json-and-go -
	// Generic JSON with interface{}
	if chaincodeCtorJSON != "{}" {
		var f interface{}
		err := json.Unmarshal([]byte(chaincodeCtorJSON), &f)
		if err != nil {
			return errors.Wrap(err, "chaincode argument error")
		}
		m := f.(map[string]interface{})
		sm := make(map[string]interface{})
		for k := range m {
			sm[strings.ToLower(k)] = m[k]
		}
		_, argsPresent := sm["args"]
		_, funcPresent := sm["function"]
		if !argsPresent || (len(m) == 2 && !funcPresent) || len(m) > 2 {
			return errors.New("non-empty JSON chaincode parameters must contain the following keys: 'Args' or 'Function' and 'Args'")
		}
	} else {
		if cmd == nil || (cmd != chaincodeInstallCmd && cmd != chaincodePackageCmd) {
			return errors.New("empty JSON chaincode parameters must contain the following keys: 'Args' or 'Function' and 'Args'")
		}
	}

	return nil
}

func validatePeerConnectionParameters(cmdName string) error {
	if connectionProfile != common.UndefinedParamValue {
		networkConfig, err := common.GetConfig(connectionProfile)
		if err != nil {
			return err
		}
		if len(networkConfig.Channels[channelID].Peers) != 0 {
			peerAddresses = []string{}
			tlsRootCertFiles = []string{}
			for peer, peerChannelConfig := range networkConfig.Channels[channelID].Peers {
				if peerChannelConfig.EndorsingPeer {
					peerConfig, ok := networkConfig.Peers[peer]
					if !ok {
						return errors.Errorf("peer '%s' is defined in the channel config but doesn't have associated peer config", peer)
					}
					peerAddresses = append(peerAddresses, peerConfig.URL)
					tlsRootCertFiles = append(tlsRootCertFiles, peerConfig.TLSCACerts.Path)
				}
			}
		}
	}

	// currently only support multiple peer addresses for invoke
	multiplePeersAllowed := map[string]bool{
		"invoke": true,
	}
	_, ok := multiplePeersAllowed[cmdName]
	if !ok && len(peerAddresses) > 1 {
		return errors.Errorf("'%s' command can only be executed against one peer. received %d", cmdName, len(peerAddresses))
	}

	if len(tlsRootCertFiles) > len(peerAddresses) {
		logger.Warningf("received more TLS root cert files (%d) than peer addresses (%d)", len(tlsRootCertFiles), len(peerAddresses))
	}

	if viper.GetBool("peer.tls.enabled") {
		if len(tlsRootCertFiles) != len(peerAddresses) {
			return errors.Errorf("number of peer addresses (%d) does not match the number of TLS root cert files (%d)", len(peerAddresses), len(tlsRootCertFiles))
		}
	} else {
		tlsRootCertFiles = nil
	}

	return nil
}

// ChaincodeCmdFactory holds the clients used by ChaincodeCmd
type ChaincodeCmdFactory struct {
	EndorserClients []pb.EndorserClient
	DeliverClients  []pb.DeliverClient
	Certificate     tls.Certificate
	Signer          identity.SignerSerializer
	BroadcastClient common.BroadcastClient
}

// InitCmdFactory init the ChaincodeCmdFactory with default clients
func InitCmdFactory(cmdName string, isEndorserRequired, isOrdererRequired bool, cryptoProvider bccsp.BCCSP) (*ChaincodeCmdFactory, error) {
	var err error
	var endorserClients []pb.EndorserClient
	var deliverClients []pb.DeliverClient
	if isEndorserRequired {
		if err = validatePeerConnectionParameters(cmdName); err != nil {
			return nil, errors.WithMessage(err, "error validating peer connection parameters")
		}
		for i, address := range peerAddresses {
			var tlsRootCertFile string
			if tlsRootCertFiles != nil {
				tlsRootCertFile = tlsRootCertFiles[i]
			}
			endorserClient, err := common.GetEndorserClientFnc(address, tlsRootCertFile)
			if err != nil {
				return nil, errors.WithMessagef(err, "error getting endorser client for %s", cmdName)
			}
			endorserClients = append(endorserClients, endorserClient)
			deliverClient, err := common.GetPeerDeliverClientFnc(address, tlsRootCertFile)
			if err != nil {
				return nil, errors.WithMessagef(err, "error getting deliver client for %s", cmdName)
			}
			deliverClients = append(deliverClients, deliverClient)
		}
		if len(endorserClients) == 0 {
			return nil, errors.New("no endorser clients retrieved - this might indicate a bug")
		}
	}
	certificate, err := common.GetClientCertificateFnc()
	if err != nil {
		return nil, errors.WithMessage(err, "error getting client certificate")
	}

	signer, err := common.GetDefaultSignerFnc()
	if err != nil {
		return nil, errors.WithMessage(err, "error getting default signer")
	}

	var broadcastClient common.BroadcastClient
	if isOrdererRequired {
		if len(common.OrderingEndpoint) == 0 {
			if len(endorserClients) == 0 {
				return nil, errors.New("orderer is required, but no ordering endpoint or endorser client supplied")
			}
			endorserClient := endorserClients[0]

			orderingEndpoints, err := common.GetOrdererEndpointOfChainFnc(channelID, signer, endorserClient, cryptoProvider)
			if err != nil {
				return nil, errors.WithMessagef(err, "error getting channel (%s) orderer endpoint", channelID)
			}
			if len(orderingEndpoints) == 0 {
				return nil, errors.Errorf("no orderer endpoints retrieved for channel %s, pass orderer endpoint with -o flag instead", channelID)
			}
			logger.Infof("Retrieved channel (%s) orderer endpoint: %s", channelID, orderingEndpoints[0])
			// override viper env
			viper.Set("orderer.address", orderingEndpoints[0])
		}

		broadcastClient, err = common.GetBroadcastClientFnc()
		if err != nil {
			return nil, errors.WithMessage(err, "error getting broadcast client")
		}
	}
	return &ChaincodeCmdFactory{
		EndorserClients: endorserClients,
		DeliverClients:  deliverClients,
		Signer:          signer,
		BroadcastClient: broadcastClient,
		Certificate:     certificate,
	}, nil
}

// processProposals sends a signed proposal to a set of peers, and gathers all the responses.
func processProposals(endorserClients []pb.EndorserClient, signedProposal *pb.SignedProposal) ([]*pb.ProposalResponse, error) {
	responsesCh := make(chan *pb.ProposalResponse, len(endorserClients))
	errorCh := make(chan error, len(endorserClients))
	wg := sync.WaitGroup{}
	for _, endorser := range endorserClients {
		wg.Add(1)
		go func(endorser pb.EndorserClient) {
			defer wg.Done()
			proposalResp, err := endorser.ProcessProposal(context.Background(), signedProposal)
			if err != nil {
				errorCh <- err
				return
			}
			responsesCh <- proposalResp
		}(endorser)
	}
	wg.Wait()
	close(responsesCh)
	close(errorCh)
	for err := range errorCh {
		return nil, err
	}
	var responses []*pb.ProposalResponse
	for response := range responsesCh {
		responses = append(responses, response)
	}
	return responses, nil
}

// ChaincodeInvokeOrQuery invokes or queries the chaincode. If successful, the
// INVOKE form prints the ProposalResponse to STDOUT, and the QUERY form prints
// the query result on STDOUT. A command-line flag (-r, --raw) determines
// whether the query result is output as raw bytes, or as a printable string.
// The printable form is optionally (-x, --hex) a hexadecimal representation
// of the query response. If the query response is NIL, nothing is output.
//
// NOTE - Query will likely go away as all interactions with the endorser are
// Proposal and ProposalResponses
func ChaincodeInvokeOrQuery(
	spec *pb.ChaincodeSpec,
	cID string,
	txID string,
	invoke bool,
	signer identity.SignerSerializer,
	certificate tls.Certificate,
	endorserClients []pb.EndorserClient,
	deliverClients []pb.DeliverClient,
	bc common.BroadcastClient,
) (*pb.ProposalResponse, error) {
	// Build the ChaincodeInvocationSpec message
	invocation := &pb.ChaincodeInvocationSpec{ChaincodeSpec: spec}
	// logger.Info(">>>>>>>>>>>>>>>>>>>>>>> invocation:", invocation)

	creator, err := signer.Serialize()
	if err != nil {
		return nil, errors.WithMessage(err, "error serializing identity")
	}
	// logger.Info(">>>>>>>>>>>>>>>>>>>>>>> creator:", creator)

	funcName := "invoke"
	if !invoke {
		funcName = "query"
	}

	// extract the transient field if it exists
	var tMap map[string][]byte
	if transient != "" {
		if err := json.Unmarshal([]byte(transient), &tMap); err != nil {
			return nil, errors.Wrap(err, "error parsing transient string")
		}
	}

	prop, txid, err := protoutil.CreateChaincodeProposalWithTxIDAndTransient(cb.HeaderType_ENDORSER_TRANSACTION, cID, invocation, creator, txID, tMap)
	if err != nil {
		return nil, errors.WithMessagef(err, "error creating proposal for %s", funcName)
	}
	// logger.Info(">>>>>>>>>>>>>>>>>>>>>>> prop:", prop)

	signedProp, err := protoutil.GetSignedProposal(prop, signer)
	if err != nil {
		return nil, errors.WithMessagef(err, "error creating signed proposal for %s", funcName)
	}
	// logger.Info(">>>>>>>>>>>>>>>>>>>>>>> signedProp:", signedProp)

	responses, err := processProposals(endorserClients, signedProp)
	if err != nil {
		return nil, errors.WithMessagef(err, "error endorsing %s", funcName)
	}
	// logger.Info(">>>>>>>>>>>>>>>>>>>>>>> responses:", responses)

	if len(responses) == 0 {
		// this should only happen if some new code has introduced a bug
		return nil, errors.New("no proposal responses received - this might indicate a bug")
	}
	// all responses will be checked when the signed transaction is created.
	// for now, just set this so we check the first response's status
	proposalResp := responses[0]

	if invoke {
		if proposalResp != nil {
			// logger.Info(">>>>>>>>>>>>>>>>>>>>>>> proposalResp:", proposalResp)
			if proposalResp.Response.Status >= shim.ERRORTHRESHOLD {
				return proposalResp, nil
			}

			// assemble a signed transaction (it's an Envelope message)
			env, err := protoutil.CreateSignedTx(prop, signer, responses...)
			// logger.Info(">>>>>>>>>>>>>>>>>>>>>>> env:", env)
			if err != nil {
				return proposalResp, errors.WithMessage(err, "could not assemble transaction")
			}
			var dg *DeliverGroup
			var ctx context.Context
			if waitForEvent {
				var cancelFunc context.CancelFunc
				ctx, cancelFunc = context.WithTimeout(context.Background(), waitForEventTimeout)
				defer cancelFunc()

				dg = NewDeliverGroup(
					deliverClients,
					peerAddresses,
					signer,
					certificate,
					channelID,
					txid,
				)
				// connect to deliver service on all peers
				err := dg.Connect(ctx)
				if err != nil {
					return nil, err
				}
			}

			// send the envelope for ordering
			if err = bc.Send(env); err != nil {
				return proposalResp, errors.WithMessagef(err, "error sending transaction for %s", funcName)
			}

			if dg != nil && ctx != nil {
				// wait for event that contains the txid from all peers
				err = dg.Wait(ctx)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	return proposalResp, nil
}

// DeliverGroup holds all of the information needed to connect
// to a set of peers to wait for the interested txid to be
// committed to the ledgers of all peers. This functionality
// is currently implemented via the peer's DeliverFiltered service.
// An error from any of the peers/deliver clients will result in
// the invoke command returning an error. Only the first error that
// occurs will be set
type DeliverGroup struct {
	Clients     []*DeliverClient
	Certificate tls.Certificate
	ChannelID   string
	TxID        string
	Signer      identity.SignerSerializer
	mutex       sync.Mutex
	Error       error
	wg          sync.WaitGroup
}

// DeliverClient holds the client/connection related to a specific
// peer. The address is included for logging purposes
type DeliverClient struct {
	Client     pb.DeliverClient
	Connection pb.Deliver_DeliverClient
	Address    string
}

func NewDeliverGroup(
	deliverClients []pb.DeliverClient,
	peerAddresses []string,
	signer identity.SignerSerializer,
	certificate tls.Certificate,
	channelID string,
	txid string,
) *DeliverGroup {
	clients := make([]*DeliverClient, len(deliverClients))
	for i, client := range deliverClients {
		address := peerAddresses[i]
		if address == "" {
			address = viper.GetString("peer.address")
		}
		dc := &DeliverClient{
			Client:  client,
			Address: address,
		}
		clients[i] = dc
	}

	dg := &DeliverGroup{
		Clients:     clients,
		Certificate: certificate,
		ChannelID:   channelID,
		TxID:        txid,
		Signer:      signer,
	}

	return dg
}

// Connect waits for all deliver clients in the group to connect to
// the peer's deliver service, receive an error, or for the context
// to timeout. An error will be returned whenever even a single
// deliver client fails to connect to its peer
func (dg *DeliverGroup) Connect(ctx context.Context) error {
	dg.wg.Add(len(dg.Clients))
	for _, client := range dg.Clients {
		go dg.ClientConnect(ctx, client)
	}
	readyCh := make(chan struct{})
	go dg.WaitForWG(readyCh)

	select {
	case <-readyCh:
		if dg.Error != nil {
			err := errors.WithMessage(dg.Error, "failed to connect to deliver on all peers")
			return err
		}
	case <-ctx.Done():
		err := errors.New("timed out waiting for connection to deliver on all peers")
		return err
	}

	return nil
}

// ClientConnect sends a deliver seek info envelope using the
// provided deliver client, setting the deliverGroup's Error
// field upon any error
func (dg *DeliverGroup) ClientConnect(ctx context.Context, dc *DeliverClient) {
	defer dg.wg.Done()
	df, err := dc.Client.DeliverFiltered(ctx)
	if err != nil {
		err = errors.WithMessagef(err, "error connecting to deliver filtered at %s", dc.Address)
		dg.setError(err)
		return
	}
	defer df.CloseSend()
	dc.Connection = df

	envelope := createDeliverEnvelope(dg.ChannelID, dg.Certificate, dg.Signer)
	err = df.Send(envelope)
	if err != nil {
		err = errors.WithMessagef(err, "error sending deliver seek info envelope to %s", dc.Address)
		dg.setError(err)
		return
	}
}

// Wait waits for all deliver client connections in the group to
// either receive a block with the txid, an error, or for the
// context to timeout
func (dg *DeliverGroup) Wait(ctx context.Context) error {
	if len(dg.Clients) == 0 {
		return nil
	}

	dg.wg.Add(len(dg.Clients))
	for _, client := range dg.Clients {
		go dg.ClientWait(client)
	}
	readyCh := make(chan struct{})
	go dg.WaitForWG(readyCh)

	select {
	case <-readyCh:
		if dg.Error != nil {
			return dg.Error
		}
	case <-ctx.Done():
		err := errors.New("timed out waiting for txid on all peers")
		return err
	}

	return nil
}

// ClientWait waits for the specified deliver client to receive
// a block event with the requested txid
func (dg *DeliverGroup) ClientWait(dc *DeliverClient) {
	defer dg.wg.Done()
	for {
		resp, err := dc.Connection.Recv()
		if err != nil {
			err = errors.WithMessagef(err, "error receiving from deliver filtered at %s", dc.Address)
			dg.setError(err)
			return
		}
		switch r := resp.Type.(type) {
		case *pb.DeliverResponse_FilteredBlock:
			filteredTransactions := r.FilteredBlock.FilteredTransactions
			for _, tx := range filteredTransactions {
				if tx.Txid == dg.TxID {
					logger.Infof("txid [%s] committed with status (%s) at %s", dg.TxID, tx.TxValidationCode, dc.Address)
					if tx.TxValidationCode != pb.TxValidationCode_VALID {
						err = errors.Errorf("transaction invalidated with status (%s)", tx.TxValidationCode)
						dg.setError(err)
					}
					return
				}
			}
		case *pb.DeliverResponse_Status:
			err = errors.Errorf("deliver completed with status (%s) before txid received", r.Status)
			dg.setError(err)
			return
		default:
			err = errors.Errorf("received unexpected response type (%T) from %s", r, dc.Address)
			dg.setError(err)
			return
		}
	}
}

// WaitForWG waits for the deliverGroup's wait group and closes
// the channel when ready
func (dg *DeliverGroup) WaitForWG(readyCh chan struct{}) {
	dg.wg.Wait()
	close(readyCh)
}

// setError serializes an error for the deliverGroup
func (dg *DeliverGroup) setError(err error) {
	dg.mutex.Lock()
	dg.Error = err
	dg.mutex.Unlock()
}

func createDeliverEnvelope(
	channelID string,
	certificate tls.Certificate,
	signer identity.SignerSerializer,
) *cb.Envelope {
	var tlsCertHash []byte
	// check for client certificate and create hash if present
	if len(certificate.Certificate) > 0 {
		tlsCertHash = util.ComputeSHA256(certificate.Certificate[0])
	}

	start := &ab.SeekPosition{
		Type: &ab.SeekPosition_Newest{
			Newest: &ab.SeekNewest{},
		},
	}

	stop := &ab.SeekPosition{
		Type: &ab.SeekPosition_Specified{
			Specified: &ab.SeekSpecified{
				Number: math.MaxUint64,
			},
		},
	}

	seekInfo := &ab.SeekInfo{
		Start:    start,
		Stop:     stop,
		Behavior: ab.SeekInfo_BLOCK_UNTIL_READY,
	}

	env, err := protoutil.CreateSignedEnvelopeWithTLSBinding(
		cb.HeaderType_DELIVER_SEEK_INFO,
		channelID,
		signer,
		seekInfo,
		int32(0),
		uint64(0),
		tlsCertHash,
	)
	if err != nil {
		logger.Errorf("Error signing envelope: %s", err)
		return nil
	}

	return env
}
