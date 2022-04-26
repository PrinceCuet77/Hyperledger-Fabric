package bjit

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"

	cb "github.com/hyperledger/fabric-protos-go/common"
	"github.com/hyperledger/fabric/common/flogging"
	"github.com/hyperledger/fabric/internal/configtxgen/encoder"
	"github.com/hyperledger/fabric/internal/configtxgen/genesisconfig"
	"github.com/hyperledger/fabric/protoutil"
)

var logger = flogging.MustGetLogger("bjit.cmd")

func DoOutputChannelCreateTx(conf, baseProfile *genesisconfig.Profile, channelID string, outputChannelCreateTx string) error {
	logger.Info("---cc-main.go : doOutputChannelCreateTx---")
	logger.Info("---Generating new channel configtx---")

	var configtx *cb.Envelope
	var err error
	if baseProfile == nil {
		configtx, err = encoder.MakeChannelCreationTransaction(channelID, nil, conf)
	} else {
		configtx, err = encoder.MakeChannelCreationTransactionWithSystemChannelContext(channelID, nil, conf, baseProfile)
	}
	if err != nil {
		return err
	}

	logger.Info("---Writing new channel tx---")
	err = writeFile(outputChannelCreateTx, protoutil.MarshalOrPanic(configtx), 0o640)
	if err != nil {
		return fmt.Errorf("---error writing channel create tx---: %s", err)
	}
	return nil
}

func writeFile(filename string, data []byte, perm os.FileMode) error {
	dirPath := filepath.Dir(filename)
	exists, err := dirExists(dirPath)
	if err != nil {
		return err
	}
	if !exists {
		err = os.MkdirAll(dirPath, 0o750)
		if err != nil {
			return err
		}
	}
	return ioutil.WriteFile(filename, data, perm)
}

func dirExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
