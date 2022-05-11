package cha

import (
	"github.com/hyperledger/fabric/internal/peer/channel"
	"github.com/spf13/cobra"
)

type channelInfo interface {
	channelCreation()
}

type channelThings struct {

}

func (c channelThings) channelCreation() {
	var cmd *cobra.Command
	var args []string
	channel.Create(cmd, args, nil)
}