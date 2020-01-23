package concentratord

import (
	"context"
	"sync"

	"github.com/go-zeromq/zmq4"
	"github.com/gofrs/uuid"
	"github.com/golang/protobuf/proto"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	"github.com/brocaar/chirpstack-api/go/v3/gw"
	"github.com/brocaar/chirpstack-gateway-bridge/internal/backend/events"
	"github.com/brocaar/chirpstack-gateway-bridge/internal/config"
	"github.com/brocaar/lorawan"
)

// Backend implements a ConcentratorD backend.
type Backend struct {
	eventSock   zmq4.Socket
	commandSock zmq4.Socket
	commandMux  sync.Mutex

	downlinkTXAckChan  chan gw.DownlinkTXAck
	uplinkFrameChan    chan gw.UplinkFrame
	gatewayStatsChan   chan gw.GatewayStats
	subscribeEventChan chan events.Subscribe
	disconnectChan     chan lorawan.EUI64

	crcCheck bool
}

// NewBackend creates a new Backend.
func NewBackend(conf config.Config) (*Backend, error) {
	var err error
	log.WithFields(log.Fields{
		"event_url":   conf.Backend.Concentratord.EventURL,
		"command_url": conf.Backend.Concentratord.CommandURL,
	}).Info("backend/concentratord: setting up backend")

	b := Backend{
		eventSock:   zmq4.NewSub(context.Background()),
		commandSock: zmq4.NewReq(context.Background()),

		downlinkTXAckChan:  make(chan gw.DownlinkTXAck, 1),
		uplinkFrameChan:    make(chan gw.UplinkFrame, 1),
		gatewayStatsChan:   make(chan gw.GatewayStats, 1),
		subscribeEventChan: make(chan events.Subscribe, 1),

		crcCheck: conf.Backend.Concentratord.CRCCheck,
	}

	err = b.eventSock.Dial(conf.Backend.Concentratord.EventURL)
	if err != nil {
		return nil, errors.Wrap(err, "dial event api url error")
	}

	err = b.eventSock.SetOption(zmq4.OptionSubscribe, "")
	if err != nil {
		return nil, errors.Wrap(err, "set event option error")
	}

	err = b.commandSock.Dial(conf.Backend.Concentratord.CommandURL)
	if err != nil {
		return nil, errors.Wrap(err, "dial command api url error")
	}

	gatewayID, err := b.getGatewayID()
	if err != nil {
		return nil, errors.Wrap(err, "get gateway id error")
	}

	b.subscribeEventChan <- events.Subscribe{Subscribe: true, GatewayID: gatewayID}

	go b.eventLoop()

	return &b, nil
}

func (b *Backend) getGatewayID() (lorawan.EUI64, error) {
	var gatewayID lorawan.EUI64

	bb, err := b.commandRequest("gateway_id", nil)
	if err != nil {
		return gatewayID, errors.Wrap(err, "request gateway id error")
	}

	copy(gatewayID[:], bb)

	return gatewayID, nil
}

// Close closes the backend.
func (b *Backend) Close() error {
	b.eventSock.Close()
	return nil
}

// GetDownlinkTXAckChan returns the channel for downlink tx acknowledgements.
func (b *Backend) GetDownlinkTXAckChan() chan gw.DownlinkTXAck {
	return b.downlinkTXAckChan
}

// GetGatewayStatsChan returns the channel for gateway statistics.
func (b *Backend) GetGatewayStatsChan() chan gw.GatewayStats {
	return b.gatewayStatsChan
}

// GetUplinkFrameChan returns the channel for received uplinks.
func (b *Backend) GetUplinkFrameChan() chan gw.UplinkFrame {
	return b.uplinkFrameChan
}

// GetSubscribeEventChan returns the channel for the (un)subscribe events.
func (b *Backend) GetSubscribeEventChan() chan events.Subscribe {
	return b.subscribeEventChan
}

// SendDownlinkFrame sends the given downlink frame.
func (b *Backend) SendDownlinkFrame(pl gw.DownlinkFrame) error {
	loRaModInfo := pl.GetTxInfo().GetLoraModulationInfo()
	if loRaModInfo != nil {
		loRaModInfo.Bandwidth = loRaModInfo.Bandwidth * 1000
	}

	var downlinkID uuid.UUID
	copy(downlinkID[:], pl.GetDownlinkId())

	log.WithFields(log.Fields{
		"downlink_id": downlinkID,
	}).Info("backend/concentratord: forwarding downlink command")

	bb, err := b.commandRequest("down", &pl)
	if err != nil {
		log.WithError(err).Fatal("backend/concentratord: send downlink command error")
	}
	if len(bb) == 0 {
		return errors.New("no reply receieved, check concentratord logs for error")
	}

	var ack gw.DownlinkTXAck
	if err = proto.Unmarshal(bb, &ack); err != nil {
		return errors.Wrap(err, "protobuf unmarshal error")
	}

	b.downlinkTXAckChan <- ack

	return nil
}

// ApplyConfiguration is not implemented.
func (b *Backend) ApplyConfiguration(gw.GatewayConfiguration) error {
	return nil
}

// GetRawPacketForwarderEventChan returns nil.
func (b *Backend) GetRawPacketForwarderEventChan() chan gw.RawPacketForwarderEvent {
	return nil
}

// RawPacketForwarderCommand is not implemented.
func (b *Backend) RawPacketForwarderCommand(gw.RawPacketForwarderCommand) error {
	return nil
}

func (b *Backend) commandRequest(command string, v proto.Message) ([]byte, error) {
	b.commandMux.Lock()
	defer b.commandMux.Unlock()

	var bb []byte
	var err error

	if v != nil {
		bb, err = proto.Marshal(v)
		if err != nil {
			return nil, errors.Wrap(err, "protobuf marshal error")
		}
	}

	msg := zmq4.NewMsgFrom([]byte(command), bb)
	if err = b.commandSock.SendMulti(msg); err != nil {
		return nil, errors.Wrap(err, "send command request error")
	}

	reply, err := b.commandSock.Recv()
	if err != nil {
		return nil, errors.Wrap(err, "receive command request reply error")
	}

	return reply.Bytes(), nil
}

func (b *Backend) eventLoop() {
	for {
		msg, err := b.eventSock.Recv()
		if err != nil {
			log.WithError(err).Fatal("backend/concentratord: receive event message error")
			continue
		}

		if len(msg.Frames) == 0 {
			continue
		}

		if len(msg.Frames) != 2 {
			log.WithFields(log.Fields{
				"frame_count": len(msg.Frames),
			}).Error("backend/concentratord: expected 2 frames in event message")
			continue
		}

		switch string(msg.Frames[0]) {
		case "up":
			err = b.handleUplinkFrame(msg.Frames[1])
		case "stats":
			err = b.handleGatewayStats(msg.Frames[1])
		default:
			log.WithFields(log.Fields{
				"event": string(msg.Frames[0]),
			}).Error("backend/concentratord: unexpected event received")
			continue
		}

		if err != nil {
			log.WithError(err).WithFields(log.Fields{
				"event": string(msg.Frames[0]),
			}).Error("backend/concentratord: handle event error")
		}
	}
}

func (b *Backend) handleUplinkFrame(bb []byte) error {
	var pl gw.UplinkFrame
	err := proto.Unmarshal(bb, &pl)
	if err != nil {
		return errors.Wrap(err, "protobuf unmarshal error")
	}

	var uplinkID uuid.UUID
	copy(uplinkID[:], pl.GetRxInfo().GetUplinkId())

	if b.crcCheck && pl.GetRxInfo().GetCrcStatus() != gw.CRCStatus_CRC_OK {
		log.WithFields(log.Fields{
			"uplink_id":  uplinkID,
			"crc_status": pl.GetRxInfo().GetCrcStatus(),
		}).Debug("backend/concentratord: ignoring uplink event, CRC is not valid")
		return nil
	}

	loRaModInfo := pl.GetTxInfo().GetLoraModulationInfo()
	if loRaModInfo != nil {
		loRaModInfo.Bandwidth = loRaModInfo.Bandwidth / 1000
	}

	log.WithFields(log.Fields{
		"uplink_id": uplinkID,
	}).Info("backend/concentratord: uplink event received")

	b.uplinkFrameChan <- pl

	return nil
}

func (b *Backend) handleGatewayStats(bb []byte) error {
	var pl gw.GatewayStats
	err := proto.Unmarshal(bb, &pl)
	if err != nil {
		return errors.Wrap(err, "protobuf unmarshal error")
	}

	var statsID uuid.UUID
	copy(statsID[:], pl.GetStatsId())

	log.WithFields(log.Fields{
		"stats_id": statsID,
	}).Info("backend/concentratord: stats event received")

	b.gatewayStatsChan <- pl

	return nil
}
