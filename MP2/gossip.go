package main

import (
	"encoding/gob"
	"math"
	"math/rand"
	"net"
	"reflect"
	"time"
)

/**************************** Setup Functions ****************************/
func setupNeighbor(conn net.Conn) (*nodeComm, *gob.Decoder) {
	// Called when a new Node is trying to connect to this node
	tcpDec := gob.NewDecoder(conn)
	var incoming Message
	err := tcpDec.Decode(&incoming)
	if err != nil {
		return nil, nil
	}
	myMsg := new(Message)
	*myMsg = incoming
	switch m := (*myMsg).(type) {
	case ConnectionMessage:
		node := new(nodeComm)
		node.isConnected = true
		node.nodeName = m.NodeName
		node.address = m.IPaddr + ":" + m.Port
		node.conn = conn
		node.inbox = make(chan Message, 65536)
		node.outbox = make(chan Message, 65536)
		Info.Println("setup_neighbor", m.NodeName)
		neighborMutex.Lock()
		addNeighbor(node)
		neighborMutex.Unlock()
		return node, tcpDec
	default:
		Error.Println("Got unexpected type as first messge:", incoming)
	}
	return nil, nil
}

/**************************** Go Routines ****************************/
// TODO: create a dedicated TCP thread for doing all the outgoing communications and push to it via an outbox channel
func (node *nodeComm) handleOutgoingMessages() {
	tcpEnc := gob.NewEncoder(node.conn)
	var m Message
	for m = range node.outbox {
		sendMsg := new(Message)
		*sendMsg = m
		err := tcpEnc.Encode(sendMsg)
		Info.Println("Sending", *sendMsg, "to", node.nodeName)
		if err != nil {
			Error.Println("Failed to send Message, error:", err)
			_ = node.conn.Close()
			return
		}
	}
}

// TODO: Remove this as it is unnecessary
func pollNeighbors() {
	// when called, ask neighbors about their neghbors
	m := new(DiscoveryMessage)
	*m = DiscoveryMessage{true}
	// TODO: Make this Mutex RW lock
	neighborMutex.RLock()
	for nodeName, node := range neighborMap {
		if nodeName != localNodeName && node.isConnected {
			node.outbox <- *m
		}
	}
	neighborMutex.RUnlock()
}

func (node *nodeComm) handleNodeComm(tcpDec *gob.Decoder) {
	Info.Println("Start handleNodeComm for ", node.nodeName)

	// handles all logic for communication between nodes
	go node.receiveIncomingData(tcpDec) // put messages of this conn into node.inbox
	go node.handleOutgoingMessages()

	// TODO: handle outbox goroutine responsible for all outgoing TCP comms

	// TODO: make lastSentTransactionIndex part of the node struct so you can easily reset it
	lastSentTransactionIndex := -1 // to send only new transactionIDs, need to keep track of last sent index
	lastSentBlockIndex := -1       // to send only new blocks, need to keep track of last sent index
	lastSentNodeIndex := -1        // to send only new nodes, need to keep track of last sent index

	for val := range node.inbox {
		switch m := val.(type) {
		case ConnectionMessage:
			neighborMutex.Lock()
			Info.Println("Processing Connection Message:", m, "from", node.nodeName)
			if incomingNode, exists := neighborMap[m.NodeName]; !exists {

				// TODO: you never opened the conn
				Error.Println("How Did we get here?")
				newNode := new(nodeComm)
				newNode.nodeName = m.NodeName
				newNode.address = m.IPaddr + ":" + m.Port
				newNode.inbox = make(chan Message, 65536)
				newNode.outbox = make(chan Message, 65536)
				newNode.isConnected = true
				addNeighbor(newNode)
				neighborMutex.Unlock()
				go newNode.handleNodeComm(nil)
			} else {
				// 2-way communication now established
				incomingNode.isConnected = true
				neighborMutex.Unlock()
			}

		case BatchGossipMessage:
			Info.Println("Received transaction", m, "from", node.nodeName)
			transactionMutex.Lock()
			for _, curTransaction := range m.BatchTransactions {
				addTransaction(*curTransaction)
			}
			transactionMutex.Unlock()

			blockMutex.Lock()
			for _, curBlock := range m.BatchBlocks {
				addBlock(*curBlock)
			}
			blockMutex.Unlock()

			nodeMutex.Lock()
			for _, curNode := range m.BatchNodes {
				addNode(*curNode)
			}
			nodeMutex.Unlock()

		case GossipRequestMessage:
			nodeBatch := make([]*ConnectionMessage, 0)

			nodeMutex.RLock()
			for _, nodeName := range m.NodesNeeded {
				if ind, exists := nodeMap[nodeName]; exists {
					nodeBatch = append(nodeBatch, nodeList[ind])
				} else {
					Warning.Println("This node claimed to have node:", nodeName, "but doesn't anymore!")
				}
			}
			nodeMutex.RUnlock()

			transactionBatch := make([]*TransactionMessage, 0)
			transactionMutex.RLock()
			for _, transactionID := range m.TransactionsNeeded {
				if ind, exists := transactionMap[transactionID]; exists {
					transactionBatch = append(transactionBatch, transactionList[ind])
				} else {
					Warning.Println("This node claimed to have node:", transactionID, "but doesn't anymore!")
				}
			}
			transactionMutex.RUnlock()

			blockBatch := make([]*Block, 0)
			blockMutex.RLock()
			for _, blockID := range m.BlocksNeeded {
				if ind, exists := blockMap[blockID]; exists {
					blockBatch = append(blockBatch, blockList[ind])
				} else {
					Warning.Println("This node claimed to have node:", blockID, "but doesn't anymore!")
				}
			}
			blockMutex.RUnlock()

			response := new(BatchGossipMessage)
			*response = BatchGossipMessage{transactionBatch, nodeBatch, blockBatch}
			node.outbox <- *response

		case DiscoveryMessage:
			Info.Println("Processing Discovery Message:", m, "from", node.nodeName)
			if m.Request {
				nodeMutex.RLock()
				nodesPendingTransmission := make([]string, 0)
				for ; lastSentNodeIndex < len(nodeList)-1; lastSentNodeIndex++ {
					nodesPendingTransmission = append(nodesPendingTransmission, nodeList[lastSentNodeIndex+1].NodeName)
				}
				nodeMutex.RUnlock()
				blockMutex.RLock()
				blocksPendingTransmission := make([]BlockID, 0)
				for ; lastSentBlockIndex < len(blockList)-1; lastSentBlockIndex++ {
					blocksPendingTransmission = append(blocksPendingTransmission, blockList[lastSentBlockIndex+1].BlockID)
				}
				blockMutex.RUnlock()
				transactionMutex.RLock()
				transactionsPendingTransmission := make([]TransID, 0)
				for ; lastSentTransactionIndex < len(transactionList)-1; lastSentTransactionIndex++ {
					transactionsPendingTransmission = append(transactionsPendingTransmission, transactionList[lastSentTransactionIndex+1].TransactionID)
				}
				transactionMutex.RUnlock()
				response := new(DiscoveryReplyMessage)
				*response = DiscoveryReplyMessage{nodesPendingTransmission, blocksPendingTransmission, transactionsPendingTransmission}
				node.outbox <- *response
			} else {
				panic("ERROR received DiscoveryMessage with request false")
			}

		case DiscoveryReplyMessage:
			nodesNeeded := make([]string, 0)

			nodeMutex.RLock()
			for _, nodeName := range m.NodesPendingTransmission {
				if _, exists := nodeMap[nodeName]; !exists {
					nodesNeeded = append(nodesNeeded, nodeName)
				}
			}
			nodeMutex.RUnlock()

			transactionsNeeded := make([]TransID, 0)
			transactionMutex.RLock()
			for _, transactionID := range m.TransactionsPendingTransmission {
				if _, exists := transactionMap[transactionID]; !exists {
					transactionsNeeded = append(transactionsNeeded, transactionID)
				}
			}
			transactionMutex.RUnlock()

			blocksNeeded := make([]BlockID, 0)
			blockMutex.RLock()
			for _, blockID := range m.BlocksPendingTransmission {
				if _, exists := blockMap[blockID]; !exists {
					blocksNeeded = append(blocksNeeded, blockID)
				}
			}
			blockMutex.RUnlock()

			result := new(GossipRequestMessage)
			*result = GossipRequestMessage{nodesNeeded, blocksNeeded, transactionsNeeded}
			node.outbox <- *result

		default:
			if m == "DISCONNECTED" || m == nil {
				_ = node.conn.Close()
				node.isConnected = false
				neighborMutex.Lock()
				removeNeighbor(node)
				neighborMutex.Unlock()
				Info.Println("\nreturning from handle_node_comm for ", node.nodeName)
				return
			}
			Warning.Println("Unknown Type in handleNodeComm", m, "type:", reflect.TypeOf(m))
		}
	}
	Error.Println("Outside for loop in handleNodeComm")
}

func (node *nodeComm) receiveIncomingData(tcpDec *gob.Decoder) {
	if tcpDec == nil {
		tcpDec = gob.NewDecoder(node.conn)
	}
	var err error = nil
	for err == nil {
		newM := new(Message)
		err = tcpDec.Decode(newM)
		Info.Println("Received", *newM, "from", node.nodeName)
		node.inbox <- *newM

		logBandwidth(newM, 0) // TODO: come back to this later
	}
	node.inbox <- "DISCONNECTED"
	close(node.inbox)
	Warning.Println("Closing inbox for node", node.nodeName, "Cause:", err)
}

func configureGossipProtocol() {
	//   one per NodeComm
	//   Algorithm: Every POLLINGPERIOD seconds, ask for transactionIDs, Transactions, neigbors
	//   handle messages of the following type:
	//		 - poll neighbor for Transactions (POLL:TRANSACTION_IDs)
	//     - send pull request
	//     - send Transactions upon a pull request
	//     - periodically ask neighbor for its neighbors (send a string in the format: POLL:NEIGHBORS)
	go correctNumNeighbors()
	randVal := time.Duration(rand.Intn(500)) // to reduce the stress on the network at the same time because of how I'm testing on the same system with the same clocks
	for {
		pollANeighbor()                                          // TODO: Polls a Neighbor (iterate over connectedNodes in order)
		time.Sleep((POLLINGPERIOD + randVal) * time.Millisecond) //TODO: Tune Polling Period
	}
}

func correctNumNeighbors() {
	randVal := time.Duration(rand.Intn(500)) // to reduce the stress on the network at the same time because of how I'm testing on the same system with the same clocks
	for {
		time.Sleep((POLLINGPERIOD + randVal) * time.Millisecond) //TODO: Tune Polling Period
		nodeMutex.RLock()
		numNodes := len(nodeList) - 1
		nodeMutex.RUnlock()
		desiredNumConnections := min(numNodes, int(math.Ceil(math.Log2(float64(numNodes+1))))+3)
		for i := 0; desiredNumConnections > numConns && i < min(numNodes, 10); i++ {
			if i == 0 {
				Warning.Println("Targeting having", desiredNumConnections, "connections, have", numConns)
			}
			candidateNeighbor := rand.Intn(numNodes) + 1 // Do not connect to yourself
			node := new(nodeComm)
			nodeMutex.RLock()
			node.nodeName = nodeList[candidateNeighbor].NodeName
			node.address = nodeList[candidateNeighbor].IPaddr + ":" + nodeList[candidateNeighbor].Port
			nodeMutex.RUnlock()
			neighborMutex.RLock()
			if _, exists := neighborMap[node.nodeName]; !exists {
				neighborMutex.RUnlock()
				err := connectToNode(node)
				if err == nil {
					neighborMutex.Lock()
					addNeighbor(node)
					neighborMutex.Unlock()
					go node.handleNodeComm(nil)
				}
			} else {
				neighborMutex.RUnlock()
			}
		}
	}
}

func pollANeighbor() {
	if len(neighborList) > 0 {
		m := new(DiscoveryMessage)
		*m = DiscoveryMessage{true}

		for {
			neighborMutex.RLock()
			curNeighborToPoll %= len(neighborList)
			if neighborList[curNeighborToPoll] != nil {
				neighborList[curNeighborToPoll].outbox <- *m
				neighborMutex.RUnlock()
				curNeighborToPoll++
				break
			}
			neighborMutex.RUnlock()
			curNeighborToPoll++
		}
	}
}
