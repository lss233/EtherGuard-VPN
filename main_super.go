/* SPDX-License-Identifier: MIT
 *
 * Copyright (C) 2017-2021 WireGuard LLC. All Rights Reserved.
 */

package main

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/google/shlex"

	"github.com/KusakabeSi/EtherGuardVPN/config"
	"github.com/KusakabeSi/EtherGuardVPN/conn"
	"github.com/KusakabeSi/EtherGuardVPN/device"
	"github.com/KusakabeSi/EtherGuardVPN/ipc"
	"github.com/KusakabeSi/EtherGuardVPN/path"
	"github.com/KusakabeSi/EtherGuardVPN/tap"
	yaml "gopkg.in/yaml.v2"
)

func checkNhTable(NhTable config.NextHopTable, peers []config.SuperPeerInfo) error {
	allpeer := make(map[config.Vertex]bool, len(peers))
	for _, peer1 := range peers {
		allpeer[peer1.NodeID] = true
	}
	for _, peer1 := range peers {
		for _, peer2 := range peers {
			if peer1.NodeID == peer2.NodeID {
				continue
			}
			id1 := peer1.NodeID
			id2 := peer2.NodeID
			if dst, has := NhTable[id1]; has {
				if next, has2 := dst[id2]; has2 {
					if _, hasa := allpeer[*next]; hasa {

					} else {
						return errors.New(fmt.Sprintf("NextHopTable[%v][%v]=%v which is not in the peer list", id1, id2, next))
					}
				} else {
					return errors.New(fmt.Sprintf("NextHopTable[%v][%v] not found", id1, id2))
				}
			} else {
				return errors.New(fmt.Sprintf("NextHopTable[%v] not found", id1))
			}
		}
	}
	return nil
}

func printExampleSuperConf() {
	v1 := config.Vertex(1)
	v2 := config.Vertex(2)

	sconfig := config.SuperConfig{
		NodeName:   "NodeSuper",
		PostScript: "",
		PrivKeyV4:  "mL5IW0GuqbjgDeOJuPHBU2iJzBPNKhaNEXbIGwwYWWk=",
		PrivKeyV6:  "+EdOKIoBp/EvIusHDsvXhV1RJYbyN3Qr8nxlz35wl3I=",
		ListenPort: 3000,
		LogLevel: config.LoggerInfo{
			LogLevel:    "normal",
			LogTransit:  true,
			LogControl:  true,
			LogNormal:   false,
			LogInternal: true,
			LogNTP:      false,
		},
		RePushConfigInterval: 30,
		Passwords: config.Passwords{
			ShowState: "passwd",
			AddPeer:   "passwd_addpeer",
			DelPeer:   "passwd_delpeer",
		},
		GraphRecalculateSetting: config.GraphRecalculateSetting{
			StaticMode:                false,
			JitterTolerance:           5,
			JitterToleranceMultiplier: 1.01,
			NodeReportTimeout:         70,
			TimeoutCheckInterval:      5,
			RecalculateCoolDown:       5,
		},
		NextHopTable: config.NextHopTable{
			config.Vertex(1): {
				config.Vertex(2): &v2,
			},
			config.Vertex(2): {
				config.Vertex(1): &v1,
			},
		},
		EdgeTemplate:       "example_config/super_mode/n1.yaml",
		UsePSKForInterEdge: true,
		Peers: []config.SuperPeerInfo{
			{
				NodeID:         1,
				Name:           "Node_01",
				PubKey:         "ZqzLVSbXzjppERslwbf2QziWruW3V/UIx9oqwU8Fn3I=",
				PSKey:          "iPM8FXfnHVzwjguZHRW9bLNY+h7+B1O2oTJtktptQkI=",
				AdditionalCost: 0,
			},
			{
				NodeID:         2,
				Name:           "Node_02",
				PubKey:         "dHeWQtlTPQGy87WdbUARS4CtwVaR2y7IQ1qcX4GKSXk=",
				PSKey:          "juJMQaGAaeSy8aDsXSKNsPZv/nFiPj4h/1G70tGYygs=",
				AdditionalCost: 0,
			},
		},
	}

	scprint, _ := yaml.Marshal(sconfig)
	fmt.Print(string(scprint))
	return
}

func Super(configPath string, useUAPI bool, printExample bool, bindmode string) (err error) {
	if printExample {
		printExampleSuperConf()
		return nil
	}
	var sconfig config.SuperConfig

	err = readYaml(configPath, &sconfig)
	if err != nil {
		fmt.Printf("Error read config: %v\t%v\n", configPath, err)
		return err
	}
	http_sconfig = &sconfig
	err = readYaml(sconfig.EdgeTemplate, &http_econfig_tmp)
	if err != nil {
		fmt.Printf("Error read config: %v\t%v\n", sconfig.EdgeTemplate, err)
		return err
	}
	NodeName := sconfig.NodeName
	if len(NodeName) > 32 {
		return errors.New("Node name can't longer than 32 :" + NodeName)
	}

	var logLevel int
	switch sconfig.LogLevel.LogLevel {
	case "verbose", "debug":
		logLevel = device.LogLevelVerbose
	case "error":
		logLevel = device.LogLevelError
	case "silent":
		logLevel = device.LogLevelSilent
	default:
		logLevel = device.LogLevelError
	}

	logger4 := device.NewLogger(
		logLevel,
		fmt.Sprintf("(%s) ", NodeName+"_v4"),
	)
	logger6 := device.NewLogger(
		logLevel,
		fmt.Sprintf("(%s) ", NodeName+"_v6"),
	)

	http_sconfig_path = configPath
	http_PeerState = make(map[string]*PeerState)
	http_PeerIPs = make(map[string]*HttpPeerLocalIP)
	http_PeerID2Info = make(map[config.Vertex]config.SuperPeerInfo)
	http_HashSalt = []byte(config.RandomStr(32, "Salt generate failed"))
	http_passwords = sconfig.Passwords

	http_super_chains = &path.SUPER_Events{
		Event_server_pong:     make(chan path.PongMsg, 1<<5),
		Event_server_register: make(chan path.RegisterMsg, 1<<5),
	}
	http_graph = path.NewGraph(3, true, sconfig.GraphRecalculateSetting, config.NTPinfo{}, sconfig.LogLevel)
	http_graph.SetNHTable(http_sconfig.NextHopTable, [32]byte{})
	if sconfig.GraphRecalculateSetting.StaticMode {
		err = checkNhTable(http_sconfig.NextHopTable, sconfig.Peers)
		if err != nil {
			return err
		}
	}
	thetap4, _ := tap.CreateDummyTAP()
	http_device4 = device.NewDevice(thetap4, config.SuperNodeMessage, conn.NewDefaultBind(true, false, bindmode), logger4, http_graph, true, configPath, nil, &sconfig, http_super_chains, Version)
	defer http_device4.Close()
	thetap6, _ := tap.CreateDummyTAP()
	http_device6 = device.NewDevice(thetap6, config.SuperNodeMessage, conn.NewDefaultBind(false, true, bindmode), logger6, http_graph, true, configPath, nil, &sconfig, http_super_chains, Version)
	defer http_device6.Close()
	if sconfig.PrivKeyV4 != "" {
		pk4, err := device.Str2PriKey(sconfig.PrivKeyV4)
		if err != nil {
			fmt.Println("Error decode base64 ", err)
			return err
		}
		http_device4.SetPrivateKey(pk4)
		http_device4.IpcSet("fwmark=0\n")
		http_device4.IpcSet("listen_port=" + strconv.Itoa(sconfig.ListenPort) + "\n")
		http_device4.IpcSet("replace_peers=true\n")
	}

	if sconfig.PrivKeyV6 != "" {
		pk6, err := device.Str2PriKey(sconfig.PrivKeyV6)
		if err != nil {
			fmt.Println("Error decode base64 ", err)
			return err
		}
		http_device6.SetPrivateKey(pk6)
		http_device6.IpcSet("fwmark=0\n")
		http_device6.IpcSet("listen_port=" + strconv.Itoa(sconfig.ListenPort) + "\n")
		http_device6.IpcSet("replace_peers=true\n")
	}

	for _, peerconf := range sconfig.Peers {
		err := super_peeradd(peerconf)
		if err != nil {
			return err
		}
	}
	logger4.Verbosef("Device4 started")
	logger6.Verbosef("Device6 started")

	errs := make(chan error, 1<<3)
	term := make(chan os.Signal, 1)
	if useUAPI {
		uapi4, err := startUAPI(NodeName+"_v4", logger4, http_device4, errs)
		if err != nil {
			return err
		}
		defer uapi4.Close()
		uapi6, err := startUAPI(NodeName+"_v6", logger6, http_device6, errs)
		if err != nil {
			return err
		}
		defer uapi6.Close()
	}

	go Event_server_event_hendler(http_graph, http_super_chains)
	go RoutinePushSettings(path.S2TD(sconfig.RePushConfigInterval))
	go RoutineTimeoutCheck()
	go HttpServer(sconfig.ListenPort, "/api")

	if sconfig.PostScript != "" {
		cmdarg, err := shlex.Split(sconfig.PostScript)
		if err != nil {
			return fmt.Errorf("Error parse PostScript %v\n", err)
		}
		if sconfig.LogLevel.LogInternal {
			fmt.Printf("PostScript: exec.Command(%v)\n", cmdarg)
		}
		cmd := exec.Command(cmdarg[0], cmdarg[1:]...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("exec.Command(%v) failed with %v\n", cmdarg, err)
		}
		if sconfig.LogLevel.LogInternal {
			fmt.Printf("PostScript output: %s\n", string(out))
		}
	}

	signal.Notify(term, syscall.SIGTERM)
	signal.Notify(term, os.Interrupt)
	select {
	case <-term:
	case <-errs:
	case <-http_device4.Wait():
	case <-http_device6.Wait():
	}
	logger4.Verbosef("Shutting down")
	return
}

func super_peeradd(peerconf config.SuperPeerInfo) error {
	pk, err := device.Str2PubKey(peerconf.PubKey)
	if err != nil {
		return fmt.Errorf("Error decode base64 :%v", err)
	}
	if peerconf.AdditionalCost < 0 {
		return fmt.Errorf("AdditionalCost can't smaller than zero!")
	}
	if http_sconfig.PrivKeyV4 != "" {
		var psk device.NoisePresharedKey
		if peerconf.PSKey != "" {
			psk, err = device.Str2PSKey(peerconf.PSKey)
			if err != nil {
				return fmt.Errorf("Error decode base64 :%v", err)
			}
		}
		peer4, err := http_device4.NewPeer(pk, peerconf.NodeID, false)
		if err != nil {
			return fmt.Errorf("Error create peer id :%v", err)
		}
		peer4.StaticConn = false
		if peerconf.PSKey != "" {
			peer4.SetPSK(psk)
		}
	}
	if http_sconfig.PrivKeyV6 != "" {
		var psk device.NoisePresharedKey
		if peerconf.PSKey != "" {
			psk, err = device.Str2PSKey(peerconf.PSKey)
			if err != nil {
				return fmt.Errorf("Error decode base64 :%v", err)
			}
		}
		peer6, err := http_device6.NewPeer(pk, peerconf.NodeID, false)
		if err != nil {
			return fmt.Errorf("Error create peer id :%v", err)
		}
		peer6.StaticConn = false
		if peerconf.PSKey != "" {
			peer6.SetPSK(psk)
		}
	}
	http_maps_lock.Lock()
	http_PeerID2Info[peerconf.NodeID] = peerconf
	http_PeerState[peerconf.PubKey] = &PeerState{}
	http_PeerIPs[peerconf.PubKey] = &HttpPeerLocalIP{}
	http_maps_lock.Unlock()
	return nil
}

func super_peerdel(toDelete config.Vertex) {
	http_maps_lock.RLock()
	PubKey := http_PeerID2Info[toDelete].PubKey
	http_maps_lock.RUnlock()
	UpdateErrorMsg := path.UpdateErrorMsg{
		Node_id:   toDelete,
		Action:    path.Shutdown,
		ErrorCode: 410,
		ErrorMsg:  "You've been removed from supernode.",
	}
	for i := 0; i < 10; i++ {
		body, _ := path.GetByte(&UpdateErrorMsg)
		buf := make([]byte, path.EgHeaderLen+len(body))
		header, _ := path.NewEgHeader(buf[:path.EgHeaderLen])
		header.SetSrc(config.SuperNodeMessage)
		header.SetTTL(0)
		header.SetPacketLength(uint16(len(body)))
		copy(buf[path.EgHeaderLen:], body)
		header.SetDst(toDelete)

		peer4 := http_device4.LookupPeerByStr(PubKey)
		http_device4.SendPacket(peer4, path.UpdateError, buf, device.MessageTransportOffsetContent)

		peer6 := http_device6.LookupPeerByStr(PubKey)
		http_device6.SendPacket(peer6, path.UpdateError, buf, device.MessageTransportOffsetContent)
		time.Sleep(path.S2TD(0.1))
	}
	http_device4.RemovePeerByID(toDelete)
	http_device6.RemovePeerByID(toDelete)
	http_graph.RemoveVirt(toDelete, true, false)
	http_maps_lock.Lock()
	delete(http_PeerState, PubKey)
	delete(http_PeerIPs, PubKey)
	delete(http_PeerID2Info, toDelete)
	http_maps_lock.Unlock()
}

func Event_server_event_hendler(graph *path.IG, events *path.SUPER_Events) {
	for {
		select {
		case reg_msg := <-events.Event_server_register:
			var should_push_peer bool
			var should_push_nh bool
			http_maps_lock.RLock()
			if reg_msg.Node_id < config.Special_NodeID {
				http_PeerState[http_PeerID2Info[reg_msg.Node_id].PubKey].LastSeen = time.Now()
				PubKey := http_PeerID2Info[reg_msg.Node_id].PubKey
				if bytes.Equal(http_PeerState[PubKey].NhTableState[:], reg_msg.NhStateHash[:]) == false {
					copy(http_PeerState[PubKey].NhTableState[:], reg_msg.NhStateHash[:])
					should_push_nh = true
				}
				if bytes.Equal(http_PeerState[PubKey].PeerInfoState[:], reg_msg.PeerStateHash[:]) == false {
					copy(http_PeerState[PubKey].PeerInfoState[:], reg_msg.PeerStateHash[:])
					should_push_peer = true
				}

				http_PeerIPs[PubKey].IPv4 = reg_msg.LocalV4
				http_PeerIPs[PubKey].IPv6 = reg_msg.LocalV6
			}
			var peer_state_changed bool
			http_PeerInfo, http_PeerInfo_hash, peer_state_changed = get_api_peers(http_PeerInfo_hash)
			http_maps_lock.RUnlock()
			if should_push_peer || peer_state_changed {
				PushPeerinfo(false)
			}
			if should_push_nh {
				PushNhTable(false)
			}
		case pong_msg := <-events.Event_server_pong:
			var changed bool
			http_maps_lock.RLock()
			if pong_msg.Src_nodeID < config.Special_NodeID && pong_msg.Dst_nodeID < config.Special_NodeID {
				changed = graph.UpdateLatency(pong_msg.Src_nodeID, pong_msg.Dst_nodeID, pong_msg.Timediff, http_PeerID2Info[pong_msg.Dst_nodeID].AdditionalCost, true, true)
			} else {
				if http_graph.CheckAnyShouldUpdate() {
					changed = http_graph.RecalculateNhTable(true)
				}
			}
			http_maps_lock.RUnlock()
			if changed {
				NhTable := graph.GetNHTable(true)
				NhTablestr, _ := json.Marshal(NhTable)
				md5_hash_raw := md5.Sum(append(http_NhTableStr, http_HashSalt...))
				new_hash_str := hex.EncodeToString(md5_hash_raw[:])
				new_hash_str_byte := []byte(new_hash_str)
				copy(http_NhTable_Hash[:], new_hash_str_byte)
				copy(graph.NhTableHash[:], new_hash_str_byte)
				http_NhTableStr = NhTablestr
				PushNhTable(false)
			}
		}
	}
}

func RoutinePushSettings(interval time.Duration) {
	force := false
	var lastforce time.Time
	for {
		if time.Now().After(lastforce.Add(interval)) {
			lastforce = time.Now()
			force = true
		} else {
			force = false
		}
		PushNhTable(force)
		PushPeerinfo(force)
		time.Sleep(path.S2TD(1))
	}
}

func RoutineTimeoutCheck() {
	for {
		http_super_chains.Event_server_register <- path.RegisterMsg{
			Node_id: config.SuperNodeMessage,
			Version: "dummy",
		}
		http_super_chains.Event_server_pong <- path.PongMsg{
			RequestID:  0,
			Src_nodeID: config.SuperNodeMessage,
			Dst_nodeID: config.SuperNodeMessage,
		}
		time.Sleep(http_graph.TimeoutCheckInterval)
	}
}

func PushNhTable(force bool) {
	body, err := path.GetByte(path.UpdateNhTableMsg{
		State_hash: http_NhTable_Hash,
	})
	if err != nil {
		fmt.Println("Error get byte")
		return
	}
	buf := make([]byte, path.EgHeaderLen+len(body))
	header, _ := path.NewEgHeader(buf[:path.EgHeaderLen])
	header.SetDst(config.SuperNodeMessage)
	header.SetPacketLength(uint16(len(body)))
	header.SetSrc(config.SuperNodeMessage)
	header.SetTTL(0)
	copy(buf[path.EgHeaderLen:], body)
	http_maps_lock.RLock()
	for pkstr, peerstate := range http_PeerState {
		isAlive := peerstate.LastSeen.Add(path.S2TD(http_sconfig.GraphRecalculateSetting.NodeReportTimeout)).After(time.Now())
		if !isAlive {
			continue
		}
		if force || peerstate.NhTableState != http_NhTable_Hash {
			if peer := http_device4.LookupPeerByStr(pkstr); peer != nil && peer.GetEndpointDstStr() != "" {
				http_device4.SendPacket(peer, path.UpdateNhTable, buf, device.MessageTransportOffsetContent)
			}
			if peer := http_device6.LookupPeerByStr(pkstr); peer != nil && peer.GetEndpointDstStr() != "" {
				http_device6.SendPacket(peer, path.UpdateNhTable, buf, device.MessageTransportOffsetContent)
			}
		}
	}
	http_maps_lock.RUnlock()
}

func PushPeerinfo(force bool) {
	body, err := path.GetByte(path.UpdatePeerMsg{
		State_hash: http_PeerInfo_hash,
	})
	if err != nil {
		fmt.Println("Error get byte")
		return
	}
	buf := make([]byte, path.EgHeaderLen+len(body))
	header, _ := path.NewEgHeader(buf[:path.EgHeaderLen])
	header.SetDst(config.SuperNodeMessage)
	header.SetPacketLength(uint16(len(body)))
	header.SetSrc(config.SuperNodeMessage)
	header.SetTTL(0)
	copy(buf[path.EgHeaderLen:], body)
	http_maps_lock.RLock()
	for pkstr, peerstate := range http_PeerState {
		isAlive := peerstate.LastSeen.Add(path.S2TD(http_sconfig.GraphRecalculateSetting.NodeReportTimeout)).After(time.Now())
		if !isAlive {
			continue
		}
		if force || peerstate.PeerInfoState != http_PeerInfo_hash {
			if peer := http_device4.LookupPeerByStr(pkstr); peer != nil {
				http_device4.SendPacket(peer, path.UpdatePeer, buf, device.MessageTransportOffsetContent)
			}
			if peer := http_device6.LookupPeerByStr(pkstr); peer != nil {
				http_device6.SendPacket(peer, path.UpdatePeer, buf, device.MessageTransportOffsetContent)
			}
		}
	}
	http_maps_lock.RUnlock()
}

func startUAPI(interfaceName string, logger *device.Logger, the_device *device.Device, errs chan error) (net.Listener, error) {
	fileUAPI, err := func() (*os.File, error) {
		uapiFdStr := os.Getenv(ENV_EG_UAPI_FD)
		if uapiFdStr == "" {
			return ipc.UAPIOpen(interfaceName)
		}
		// use supplied fd
		fd, err := strconv.ParseUint(uapiFdStr, 10, 32)
		if err != nil {
			return nil, err
		}
		return os.NewFile(uintptr(fd), ""), nil
	}()
	if err != nil {
		fmt.Printf("Error create UAPI socket \n")
		return nil, err
	}
	uapi, err := ipc.UAPIListen(interfaceName, fileUAPI)
	if err != nil {
		logger.Errorf("Failed to listen on uapi socket: %v", err)
		return nil, err
	}

	go func() {
		for {
			conn, err := uapi.Accept()
			if err != nil {
				errs <- err
				return
			}
			go the_device.IpcHandle(conn)
		}
	}()
	logger.Verbosef("UAPI listener started")
	return uapi, err
}
