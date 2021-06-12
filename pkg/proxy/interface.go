package proxy

import (
	"github.com/jumpserver/koko/pkg/exchange"
)

//type proxyEngine interface {
//	GenerateRecordCommand(s *commonSwitch, input, output string, riskLevel int64) *model.Command
//
//	//NewParser(s *commonSwitch) ParseEngine
//
//	MapData(s *commonSwitch) map[string]interface{}
//
//	CheckPermissionExpired(time.Time) bool
//}

type ParseEngine interface {
	ParseStream(userInChan chan *exchange.RoomMessage, srvInChan <-chan []byte) (userOut, srvOut <-chan []byte)

	Close()

	NeedRecord() bool

	CommandRecordChan() chan *ExecutedCommand
}
