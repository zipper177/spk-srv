//go:generate go-bindata -nocompress voices/dmr voices/dstar voices/p25

package main

import (
	"io"
	"log"
	"flag"
	"net"
	"strings"
	"encoding/binary"
	"bytes"
	"path/filepath"
	"os"
	"time"
	"fmt"
	"io/ioutil"
)

func getFilePathForCodePair(modemMode spkModemMode, codePair string) string {
	var dir string

	switch (modemMode) {
		case SPK_MODEM_MODE_DMR: dir = "voices/dmr/"
		case SPK_MODEM_MODE_DSTAR: dir = "voices/dstar/"
		case SPK_MODEM_MODE_C4FM: dir = "voices/dmr/"
		case SPK_MODEM_MODE_C4FM_HALF_DEVIATION: dir = "voices/dmr/"
		case SPK_MODEM_MODE_NXDN: dir = "voices/dmr/"
		case SPK_MODEM_MODE_P25: dir = "voices/p25/"
	}
	if dir == "" {
		return dir
	}

	for filePath, _ := range _bindata {
		if filepath.Dir(filePath) != filepath.Clean(dir) || filepath.Ext(filePath) != ".ambe" {
			continue
		}

		fileName := filepath.Base(filePath)
		// The requested code pair is stored in the first two characters of the filename.
		var fileCodePair = string(fileName[0]) + string(fileName[1])
		if fileCodePair == codePair {
			return filePath
		}
	}
	return ""
}

func sendAMBEAnswer(udpConn *net.UDPConn, toAddr *net.UDPAddr, res *spkAMBEResponsePacket) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, res)
	writtenBytes, err := udpConn.WriteToUDP(buf.Bytes(), toAddr)
	if writtenBytes != SPK_AMBE_RESPONSE_PACKET_SIZE || err != nil {
		log.Printf("warning: can't send udp packet to %s\n", toAddr.String())
	}

	if res.PacketType != SPK_PACKET_TYPE_RESPONSE_TERMINATOR {
		res.SeqNum++
		time.Sleep(time.Duration(res.FrameCount) * 20 * time.Millisecond)
	}
}

func sendIMBEAnswer(udpConn *net.UDPConn, toAddr *net.UDPAddr, res *spkIMBEResponsePacket) {
	var buf bytes.Buffer
	binary.Write(&buf, binary.BigEndian, res)
	writtenBytes, err := udpConn.WriteToUDP(buf.Bytes(), toAddr)
	if writtenBytes != SPK_IMBE_RESPONSE_PACKET_SIZE || err != nil {
		log.Printf("warning: can't send udp packet to %s\n", toAddr.String())
	}

	if res.PacketType != SPK_PACKET_TYPE_RESPONSE_TERMINATOR {
		res.SeqNum++
		time.Sleep(time.Duration(res.FrameCount) * 20 * time.Millisecond)
	}
}

func StartSendAnswer(udpConn *net.UDPConn, toAddr net.UDPAddr, rp *spkRequestPacket) {
	defer RequestRemove(rp.SessionID, &toAddr)

	codeStr := strings.TrimRight(string(rp.CodeStr[:]), "\x00")

	// If the client is requesting a connect announce to a Homebrew server, we try to query a BM status from
	// the server's BM HTTP API to get linked talkgroups and reflector.
	bmGetClientDataFinished := make(chan bool)
	defer close(bmGetClientDataFinished)
	bmGetClientDataRunning := false
	var bmGetClientDataResult bmClientData
	var serverData bmServerData
	if rp.ConnectorID == SPK_CONNECTOR_ID_HOMEBREW &&
		(rp.AnnounceType == SPK_ANNOUNCE_TYPE_CONNECTED || rp.AnnounceType == SPK_ANNOUNCE_TYPE_CONNECTED_BRANDMEISTER_SHORTENED ||
			rp.AnnounceType == SPK_ANNOUNCE_TYPE_CONNECTOR_STATUS) {

		serverIP := fmt.Sprintf("%d.%d.%d.%d", rp.AnnounceTypeData[0] >> 24, (rp.AnnounceTypeData[0] >> 16) & 0xff,
			(rp.AnnounceTypeData[0] >> 8) & 0xff, rp.AnnounceTypeData[0] & 0xff)

		var ok bool
		if serverData, ok = BMGetServerDataForServerIP(serverIP); ok {
			clientId := rp.AnnounceTypeData[1]

			log.Printf("getting bm client data srv:%s cid:%d", serverIP, clientId)
			bmGetClientDataRunning = true
			go BMGetClientData(clientId, &bmGetClientDataResult, bmGetClientDataFinished)
		}
	}

	var res spkResponsePacket
	switch (rp.ModemMode) {
		default:
			copy(res.AMBE.Magic[:], SPK_PACKET_MAGIC)
			res.AMBE.PacketType = SPK_PACKET_TYPE_AMBE_RESPONSE
			res.AMBE.SessionID = rp.SessionID
			break
		case SPK_MODEM_MODE_P25:
			copy(res.IMBE.Magic[:], SPK_PACKET_MAGIC)
			res.IMBE.PacketType = SPK_PACKET_TYPE_IMBE_RESPONSE
			res.IMBE.SessionID = rp.SessionID
			break
	}

	// Stepping through each code char pair.
	for codeStrPos := 0; codeStrPos < len(codeStr); codeStrPos += 2 {
		if bmGetClientDataRunning {
			select {
				case finished := <- bmGetClientDataFinished:
					if finished && codeStrPos < 4 {
						codeStr = strings.Replace(codeStr, "HBSV", BMGenerateCodeStrFromClientData(&bmGetClientDataResult, &serverData,
							rp.AnnounceType == SPK_ANNOUNCE_TYPE_CONNECTED_BRANDMEISTER_SHORTENED), 1)
						log.Printf("code str modified for %s to %s", toAddr.String(), codeStr)
						bmGetClientDataRunning = false
					}
				default:
					break
			}
		}

		if codeStrPos+2 > len(codeStr) {
			log.Println("warning: last code pair is broken")
			break
		}

		var codePair = codeStr[codeStrPos:codeStrPos+2]

		filePath := getFilePathForCodePair(rp.ModemMode, codePair)
		if filePath == "" {
			log.Printf("warning: file not found for modem mode %d code pair \"%s\", skipping\n", rp.ModemMode, codePair)
			continue
		}

		data, err := Asset(filePath)
		if err != nil {
			log.Printf("warning: can't find \"%s\", skipping\n", filePath)
			continue
		}

		log.Printf("playing %s to %s\n", filePath, toAddr.String())

		reader := bytes.NewReader(data)
		var fileFinished = false

		// Filling up frames from the file.
		for !fileFinished {
			switch (rp.ModemMode) {
				default:
					for ; res.AMBE.FrameCount < 3; res.AMBE.FrameCount++ {
						readBytes, err := reader.Read(res.AMBE.Frames[res.AMBE.FrameCount][:])
						if err != nil || readBytes != 9 {
							fileFinished = true
							break
						}
					}

					// Flushing if needed.
					if res.AMBE.FrameCount == 3 {
						sendAMBEAnswer(udpConn, &toAddr, &res.AMBE)
						res.AMBE.FrameCount = 0
					}
					break
				case SPK_MODEM_MODE_P25:
					for ; res.IMBE.FrameCount < 3; res.IMBE.FrameCount++ {
						readBytes, err := reader.Read(res.IMBE.Frames[res.IMBE.FrameCount][:])
						if err != nil || readBytes != 18 {
							fileFinished = true
							break
						}
					}

					// Flushing if needed.
					if res.IMBE.FrameCount == 3 {
						sendIMBEAnswer(udpConn, &toAddr, &res.IMBE)
						res.IMBE.FrameCount = 0
					}
					break
			}
		}
	}

	switch (rp.ModemMode) {
		default:
			res.AMBE.PacketType = SPK_PACKET_TYPE_RESPONSE_TERMINATOR
			sendAMBEAnswer(udpConn, &toAddr, &res.AMBE)
			break;
		case SPK_MODEM_MODE_P25:
			res.IMBE.PacketType = SPK_PACKET_TYPE_RESPONSE_TERMINATOR
			sendIMBEAnswer(udpConn, &toAddr, &res.IMBE)
			break
	}

	if bmGetClientDataRunning {
		<- bmGetClientDataFinished
	}

	log.Printf("playing to %s finished\n", toAddr.String())
}

func processPacket(udpConn *net.UDPConn, fromAddr *net.UDPAddr, buffer []byte, readBytes int) {
	var packetType = buffer[7]

	switch (packetType) {
		default:
			log.Printf("ignoring packet with type 0x%.2x\n", packetType)
		case SPK_PACKET_TYPE_REQUEST:
			if readBytes != SPK_REQUEST_PACKET_SIZE {
				log.Printf("ignoring packet with size %d\n", readBytes)
				return
			}

			// Reading the packet payload to our request struct.
			readBuf := bytes.NewReader(buffer)
			var rp spkRequestPacket
			err := binary.Read(readBuf, binary.BigEndian, &rp)
			if err != nil {
				log.Println("ignoring packet, binary parse error: ", err)
				return
			}

			switch (rp.ModemMode) {
				case SPK_MODEM_MODE_DMR: break
				case SPK_MODEM_MODE_DSTAR: break
				case SPK_MODEM_MODE_C4FM: break
				case SPK_MODEM_MODE_C4FM_HALF_DEVIATION: break
				case SPK_MODEM_MODE_NXDN: break
				case SPK_MODEM_MODE_P25: break
				default:
					log.Printf("ignoring packet, invalid modem mode %.2x\n", rp.ModemMode)
					return
			}

			rp.CodeStr[SPK_ANNOUNCE_DATA_MAX_LENGTH-1] = 0

			if RequestIsAdded(rp.SessionID, fromAddr) {
				//log.Printf("ignoring packet, request already under processing with sid:0x%.8x\n", rp.SessionID)
				return
			}
			RequestAdd(rp.SessionID, fromAddr)

			atStr, atdStr := decodeAnnounceTypeAndDataToStr(rp.AnnounceType, rp.AnnounceTypeData)
			log.Printf("sending \"%s\" to %s (sid:0x%.8x t:%s con:%s at:%s %s)\n",
				strings.TrimRight(string(rp.CodeStr[:]), "\x00"), fromAddr.String(), rp.SessionID, getModemModeNameStr(rp.ModemMode),
				getConnectorIdNameStr(rp.ConnectorID), atStr, atdStr);
			go StartSendAnswer(udpConn, *fromAddr, &rp)
	}
}

func main() {
	var bindIp = "0.0.0.0"
	var bindPort = 65200
	var silent bool
	var logToFile bool

	flag.IntVar(&bindPort, "p", bindPort, "bind to port")
	flag.StringVar(&bindIp, "i", bindIp, "bind to ip address")
	flag.BoolVar(&silent, "s", false, "disable logging")
	flag.BoolVar(&logToFile, "f", false, "log to file spk-srv.log")
	flag.Parse()

	if logToFile && !silent {
		logFile, err := os.OpenFile("spk-srv.log", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0600)
		if err != nil {
			log.Println("warning: can't open spk-srv.log for writing: ", err)
		} else {
			defer logFile.Close()
			log.SetOutput(io.MultiWriter(os.Stdout, logFile))
		}
	}

	log.Printf("spk-srv start, binding to %s:%d\n", bindIp, bindPort)

	if silent {
		log.SetFlags(0)
		log.SetOutput(ioutil.Discard)
	}

	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{ net.ParseIP(bindIp), bindPort, "" })
	if err != nil {
		log.Fatal(err)
	}
	defer udpConn.Close()

	go BMProcess()

	log.Println("starting listening loop")
	buffer := make([]byte, 128)
	for {
		readBytes, fromAddr, err := udpConn.ReadFromUDP(buffer)
		if err != nil {
			log.Fatal(err)
		}

		// Did we read at least magic + version number of bytes? Does packet magic match?
		if readBytes >= 7 && strings.Compare(SPK_PACKET_MAGIC, string(buffer[:6])) == 0 {
			log.Printf("got %d byte packet from %s\n", readBytes, fromAddr.String())

			switch (buffer[6]) {
				default:
					log.Printf("ignoring packet with version 0x%.2x\n", buffer[6])
				case 0:
					processPacket(udpConn, fromAddr, buffer, readBytes)
			}
		}
	}
}
