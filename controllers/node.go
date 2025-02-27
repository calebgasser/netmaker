package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/gravitl/netmaker/database"
	"github.com/gravitl/netmaker/functions"
	"github.com/gravitl/netmaker/logger"
	"github.com/gravitl/netmaker/logic"
	"github.com/gravitl/netmaker/models"
	"github.com/gravitl/netmaker/mq"
	"github.com/gravitl/netmaker/servercfg"
	"golang.org/x/crypto/bcrypt"
)

func nodeHandlers(r *mux.Router) {

	r.HandleFunc("/api/nodes", authorize(false, false, "user", http.HandlerFunc(getAllNodes))).Methods("GET")
	r.HandleFunc("/api/nodes/{network}", authorize(false, true, "network", http.HandlerFunc(getNetworkNodes))).Methods("GET")
	r.HandleFunc("/api/nodes/{network}/{nodeid}", authorize(true, true, "node", http.HandlerFunc(getNode))).Methods("GET")
	r.HandleFunc("/api/nodes/{network}/{nodeid}", authorize(false, true, "node", http.HandlerFunc(updateNode))).Methods("PUT")
	r.HandleFunc("/api/nodes/{network}/{nodeid}", authorize(true, true, "node", http.HandlerFunc(deleteNode))).Methods("DELETE")
	r.HandleFunc("/api/nodes/{network}/{nodeid}/createrelay", authorize(false, true, "user", http.HandlerFunc(createRelay))).Methods("POST")
	r.HandleFunc("/api/nodes/{network}/{nodeid}/deleterelay", authorize(false, true, "user", http.HandlerFunc(deleteRelay))).Methods("DELETE")
	r.HandleFunc("/api/nodes/{network}/{nodeid}/creategateway", authorize(false, true, "user", http.HandlerFunc(createEgressGateway))).Methods("POST")
	r.HandleFunc("/api/nodes/{network}/{nodeid}/deletegateway", authorize(false, true, "user", http.HandlerFunc(deleteEgressGateway))).Methods("DELETE")
	r.HandleFunc("/api/nodes/{network}/{nodeid}/createingress", securityCheck(false, http.HandlerFunc(createIngressGateway))).Methods("POST")
	r.HandleFunc("/api/nodes/{network}/{nodeid}/deleteingress", securityCheck(false, http.HandlerFunc(deleteIngressGateway))).Methods("DELETE")
	r.HandleFunc("/api/nodes/{network}/{nodeid}/approve", authorize(false, true, "user", http.HandlerFunc(uncordonNode))).Methods("POST")
	r.HandleFunc("/api/nodes/{network}", nodeauth(http.HandlerFunc(createNode))).Methods("POST")
	r.HandleFunc("/api/nodes/adm/{network}/lastmodified", authorize(false, true, "network", http.HandlerFunc(getLastModified))).Methods("GET")
	r.HandleFunc("/api/nodes/adm/{network}/authenticate", authenticate).Methods("POST")
}

func authenticate(response http.ResponseWriter, request *http.Request) {

	var authRequest models.AuthParams
	var result models.Node
	var errorResponse = models.ErrorResponse{
		Code: http.StatusInternalServerError, Message: "W1R3: It's not you it's me.",
	}

	decoder := json.NewDecoder(request.Body)
	decoderErr := decoder.Decode(&authRequest)
	defer request.Body.Close()

	if decoderErr != nil {
		errorResponse.Code = http.StatusBadRequest
		errorResponse.Message = decoderErr.Error()
		returnErrorResponse(response, request, errorResponse)
		return
	} else {
		errorResponse.Code = http.StatusBadRequest
		if authRequest.ID == "" {
			errorResponse.Message = "W1R3: ID can't be empty"
			returnErrorResponse(response, request, errorResponse)
			return
		} else if authRequest.Password == "" {
			errorResponse.Message = "W1R3: Password can't be empty"
			returnErrorResponse(response, request, errorResponse)
			return
		} else {
			var err error
			result, err = logic.GetNodeByID(authRequest.ID)

			if err != nil {
				errorResponse.Code = http.StatusBadRequest
				errorResponse.Message = err.Error()
				returnErrorResponse(response, request, errorResponse)
				return
			}

			err = bcrypt.CompareHashAndPassword([]byte(result.Password), []byte(authRequest.Password))
			if err != nil {
				errorResponse.Code = http.StatusBadRequest
				errorResponse.Message = err.Error()
				returnErrorResponse(response, request, errorResponse)
				return
			} else {
				tokenString, _ := logic.CreateJWT(authRequest.ID, authRequest.MacAddress, result.Network)

				if tokenString == "" {
					errorResponse.Code = http.StatusBadRequest
					errorResponse.Message = "Could not create Token"
					returnErrorResponse(response, request, errorResponse)
					return
				}

				var successResponse = models.SuccessResponse{
					Code:    http.StatusOK,
					Message: "W1R3: Device " + authRequest.ID + " Authorized",
					Response: models.SuccessfulLoginResponse{
						AuthToken: tokenString,
						ID:        authRequest.ID,
					},
				}
				successJSONResponse, jsonError := json.Marshal(successResponse)

				if jsonError != nil {
					errorResponse.Code = http.StatusBadRequest
					errorResponse.Message = err.Error()
					returnErrorResponse(response, request, errorResponse)
					return
				}
				response.WriteHeader(http.StatusOK)
				response.Header().Set("Content-Type", "application/json")
				response.Write(successJSONResponse)
			}
		}
	}
}

// auth middleware for api calls from nodes where node is has not yet joined the server (register, join)
func nodeauth(next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bearerToken := r.Header.Get("Authorization")
		var tokenSplit = strings.Split(bearerToken, " ")
		var token = ""
		if len(tokenSplit) < 2 {
			errorResponse := models.ErrorResponse{
				Code: http.StatusUnauthorized, Message: "W1R3: You are unauthorized to access this endpoint.",
			}
			returnErrorResponse(w, r, errorResponse)
			return
		} else {
			token = tokenSplit[1]
		}
		found := false
		networks, err := logic.GetNetworks()
		if err != nil {
			logger.Log(0, "no networks", err.Error())
			errorResponse := models.ErrorResponse{
				Code: http.StatusNotFound, Message: "no networks",
			}
			returnErrorResponse(w, r, errorResponse)
			return
		}
		for _, network := range networks {
			for _, key := range network.AccessKeys {
				if key.Value == token {
					found = true
					break
				}
			}
		}
		if !found {
			logger.Log(0, "valid access key not found")
			errorResponse := models.ErrorResponse{
				Code: http.StatusUnauthorized, Message: "You are unauthorized to access this endpoint.",
			}
			returnErrorResponse(w, r, errorResponse)
			return
		}
		next.ServeHTTP(w, r)
	}
}

//The middleware for most requests to the API
//They all pass  through here first
//This will validate the JWT (or check for master token)
//This will also check against the authNetwork and make sure the node should be accessing that endpoint,
//even if it's technically ok
//This is kind of a poor man's RBAC. There's probably a better/smarter way.
//TODO: Consider better RBAC implementations
func authorize(nodesAllowed, networkCheck bool, authNetwork string, next http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var errorResponse = models.ErrorResponse{
			Code: http.StatusInternalServerError, Message: "W1R3: It's not you it's me.",
		}

		var params = mux.Vars(r)

		networkexists, _ := functions.NetworkExists(params["network"])
		//check that the request is for a valid network
		//if (networkCheck && !networkexists) || err != nil {
		if networkCheck && !networkexists {
			errorResponse = models.ErrorResponse{
				Code: http.StatusNotFound, Message: "W1R3: This network does not exist. ",
			}
			returnErrorResponse(w, r, errorResponse)
			return
		} else {
			w.Header().Set("Content-Type", "application/json")

			//get the auth token
			bearerToken := r.Header.Get("Authorization")

			var tokenSplit = strings.Split(bearerToken, " ")

			//I put this in in case the user doesn't put in a token at all (in which case it's empty)
			//There's probably a smarter way of handling this.
			var authToken = "928rt238tghgwe@TY@$Y@#WQAEGB2FC#@HG#@$Hddd"

			if len(tokenSplit) > 1 {
				authToken = tokenSplit[1]
			} else {
				errorResponse = models.ErrorResponse{
					Code: http.StatusUnauthorized, Message: "W1R3: Missing Auth Token.",
				}
				returnErrorResponse(w, r, errorResponse)
				return
			}
			//check if node instead of user
			if nodesAllowed {
				// TODO --- should ensure that node is only operating on itself
				if _, _, _, err := logic.VerifyToken(authToken); err == nil {
					next.ServeHTTP(w, r)
					return
				}
			}

			var isAuthorized = false
			var nodeID = ""
			username, networks, isadmin, errN := logic.VerifyUserToken(authToken)
			if errN != nil {
				errorResponse = models.ErrorResponse{
					Code: http.StatusUnauthorized, Message: "W1R3: Unauthorized, Invalid Token Processed.",
				}
				returnErrorResponse(w, r, errorResponse)
				return
			}
			isnetadmin := isadmin
			if errN == nil && isadmin {
				nodeID = "mastermac"
				isAuthorized = true
				r.Header.Set("ismasterkey", "yes")
			}
			if !isadmin && params["network"] != "" {
				if logic.StringSliceContains(networks, params["network"]) {
					isnetadmin = true
				}
			}
			//The mastermac (login with masterkey from config) can do everything!! May be dangerous.
			if nodeID == "mastermac" {
				isAuthorized = true
				r.Header.Set("ismasterkey", "yes")
				//for everyone else, there's poor man's RBAC. The "cases" are defined in the routes in the handlers
				//So each route defines which access network should be allowed to access it
			} else {
				switch authNetwork {
				case "all":
					isAuthorized = true
				case "nodes":
					isAuthorized = (nodeID != "") || isnetadmin
				case "network":
					if isnetadmin {
						isAuthorized = true
					} else {
						node, err := logic.GetNodeByID(nodeID)
						if err != nil {
							errorResponse = models.ErrorResponse{
								Code: http.StatusUnauthorized, Message: "W1R3: Missing Auth Token.",
							}
							returnErrorResponse(w, r, errorResponse)
							return
						}
						isAuthorized = (node.Network == params["network"])
					}
				case "node":
					if isnetadmin {
						isAuthorized = true
					} else {
						isAuthorized = (nodeID == params["netid"])
					}
				case "user":
					isAuthorized = true
				default:
					isAuthorized = false
				}
			}
			if !isAuthorized {
				errorResponse = models.ErrorResponse{
					Code: http.StatusUnauthorized, Message: "W1R3: You are unauthorized to access this endpoint.",
				}
				returnErrorResponse(w, r, errorResponse)
				return
			} else {
				//If authorized, this function passes along it's request and output to the appropriate route function.
				if username == "" {
					username = "(user not found)"
				}
				r.Header.Set("user", username)
				next.ServeHTTP(w, r)
			}
		}
	}
}

//Gets all nodes associated with network, including pending nodes
func getNetworkNodes(w http.ResponseWriter, r *http.Request) {

	w.Header().Set("Content-Type", "application/json")

	var nodes []models.Node
	var params = mux.Vars(r)
	networkName := params["network"]

	nodes, err := logic.GetNetworkNodes(networkName)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}

	//Returns all the nodes in JSON format
	logger.Log(2, r.Header.Get("user"), "fetched nodes on network", networkName)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(nodes)
}

//A separate function to get all nodes, not just nodes for a particular network.
//Not quite sure if this is necessary. Probably necessary based on front end but may want to review after iteration 1 if it's being used or not
func getAllNodes(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	user, err := logic.GetUser(r.Header.Get("user"))
	if err != nil && r.Header.Get("ismasterkey") != "yes" {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}
	var nodes []models.Node
	if user.IsAdmin || r.Header.Get("ismasterkey") == "yes" {
		nodes, err = logic.GetAllNodes()
		if err != nil {
			returnErrorResponse(w, r, formatError(err, "internal"))
			return
		}
	} else {
		nodes, err = getUsersNodes(user)
		if err != nil {
			returnErrorResponse(w, r, formatError(err, "internal"))
			return
		}
	}
	//Return all the nodes in JSON format
	logger.Log(3, r.Header.Get("user"), "fetched all nodes they have access to")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(nodes)
}

func getUsersNodes(user models.User) ([]models.Node, error) {
	var nodes []models.Node
	var err error
	for _, networkName := range user.Networks {
		tmpNodes, err := logic.GetNetworkNodes(networkName)
		if err != nil {
			continue
		}
		nodes = append(nodes, tmpNodes...)
	}
	return nodes, err
}

//Get an individual node. Nothin fancy here folks.
func getNode(w http.ResponseWriter, r *http.Request) {
	// set header.
	w.Header().Set("Content-Type", "application/json")

	var params = mux.Vars(r)

	node, err := logic.GetNodeByID(params["nodeid"])
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}

	peerUpdate, err := logic.GetPeerUpdate(&node)
	if err != nil && !database.IsEmptyRecord(err) {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}

	response := models.NodeGet{
		Node:         node,
		Peers:        peerUpdate.Peers,
		ServerConfig: servercfg.GetServerInfo(),
	}

	logger.Log(2, r.Header.Get("user"), "fetched node", params["nodeid"])
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
}

//Get the time that a network of nodes was last modified.
//TODO: This needs to be refactored
//Potential way to do this: On UpdateNode, set a new field for "LastModified"
//If we go with the existing way, we need to at least set network.NodesLastModified on UpdateNode
func getLastModified(w http.ResponseWriter, r *http.Request) {
	// set header.
	w.Header().Set("Content-Type", "application/json")

	var params = mux.Vars(r)
	network, err := logic.GetNetwork(params["network"])
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}
	logger.Log(2, r.Header.Get("user"), "called last modified")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(network.NodesLastModified)
}

func createNode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var params = mux.Vars(r)

	var errorResponse = models.ErrorResponse{
		Code: http.StatusInternalServerError, Message: "W1R3: It's not you it's me.",
	}
	networkName := params["network"]
	networkexists, err := functions.NetworkExists(networkName)

	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	} else if !networkexists {
		errorResponse = models.ErrorResponse{
			Code: http.StatusNotFound, Message: "W1R3: Network does not exist! ",
		}
		returnErrorResponse(w, r, errorResponse)
		return
	}

	var node = models.Node{}

	//get node from body of request
	err = json.NewDecoder(r.Body).Decode(&node)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}

	node.Network = networkName

	network, err := logic.GetNetworkByNode(&node)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}
	node.NetworkSettings, err = logic.GetNetworkSettings(node.Network)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}
	validKey := logic.IsKeyValid(networkName, node.AccessKey)
	if !validKey {
		// Check to see if network will allow manual sign up
		// may want to switch this up with the valid key check and avoid a DB call that way.
		if network.AllowManualSignUp == "yes" {
			node.IsPending = "yes"
		} else {
			errorResponse = models.ErrorResponse{
				Code: http.StatusUnauthorized, Message: "W1R3: Key invalid, or none provided.",
			}
			returnErrorResponse(w, r, errorResponse)
			return
		}
	}
	key, keyErr := logic.RetrievePublicTrafficKey()
	if keyErr != nil {
		logger.Log(0, "error retrieving key: ", keyErr.Error())
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}
	if key == nil {
		logger.Log(0, "error: server traffic key is nil")
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}
	if node.TrafficKeys.Mine == nil {
		logger.Log(0, "error: node traffic key is nil")
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}
	node.TrafficKeys = models.TrafficKeys{
		Mine:   node.TrafficKeys.Mine,
		Server: key,
	}

	err = logic.CreateNode(&node)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}

	peerUpdate, err := logic.GetPeerUpdate(&node)
	if err != nil && !database.IsEmptyRecord(err) {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}

	response := models.NodeGet{
		Node:         node,
		Peers:        peerUpdate.Peers,
		ServerConfig: servercfg.GetServerInfo(),
	}

	logger.Log(1, r.Header.Get("user"), "created new node", node.Name, "on network", node.Network)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(response)
	runForceServerUpdate(&node)
}

// Takes node out of pending state
// TODO: May want to use cordon/uncordon terminology instead of "ispending".
func uncordonNode(w http.ResponseWriter, r *http.Request) {
	var params = mux.Vars(r)
	w.Header().Set("Content-Type", "application/json")
	var nodeid = params["nodeid"]
	node, err := logic.UncordonNode(nodeid)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}
	logger.Log(1, r.Header.Get("user"), "uncordoned node", node.Name)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode("SUCCESS")

	runUpdates(&node, false)
}

// == EGRESS ==

func createEgressGateway(w http.ResponseWriter, r *http.Request) {
	var gateway models.EgressGatewayRequest
	var params = mux.Vars(r)
	w.Header().Set("Content-Type", "application/json")
	err := json.NewDecoder(r.Body).Decode(&gateway)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}
	gateway.NetID = params["network"]
	gateway.NodeID = params["nodeid"]
	node, err := logic.CreateEgressGateway(gateway)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}

	logger.Log(1, r.Header.Get("user"), "created egress gateway on node", gateway.NodeID, "on network", gateway.NetID)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(node)

	runUpdates(&node, true)
}

func deleteEgressGateway(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var params = mux.Vars(r)
	nodeid := params["nodeid"]
	netid := params["network"]
	node, err := logic.DeleteEgressGateway(netid, nodeid)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}

	logger.Log(1, r.Header.Get("user"), "deleted egress gateway", nodeid, "on network", netid)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(node)

	runUpdates(&node, true)
}

// == INGRESS ==

func createIngressGateway(w http.ResponseWriter, r *http.Request) {
	var params = mux.Vars(r)
	w.Header().Set("Content-Type", "application/json")
	nodeid := params["nodeid"]
	netid := params["network"]
	node, err := logic.CreateIngressGateway(netid, nodeid)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}

	logger.Log(1, r.Header.Get("user"), "created ingress gateway on node", nodeid, "on network", netid)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(node)

	runUpdates(&node, true)
}

func deleteIngressGateway(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var params = mux.Vars(r)
	nodeid := params["nodeid"]
	node, err := logic.DeleteIngressGateway(params["network"], nodeid)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}

	logger.Log(1, r.Header.Get("user"), "deleted ingress gateway", nodeid)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(node)

	runUpdates(&node, true)
}

func updateNode(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var params = mux.Vars(r)

	var node models.Node
	//start here
	node, err := logic.GetNodeByID(params["nodeid"])
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}

	var newNode models.Node
	// we decode our body request params
	err = json.NewDecoder(r.Body).Decode(&newNode)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "badrequest"))
		return
	}
	relayupdate := false
	if node.IsRelay == "yes" && len(newNode.RelayAddrs) > 0 {
		if len(newNode.RelayAddrs) != len(node.RelayAddrs) {
			relayupdate = true
		} else {
			for i, addr := range newNode.RelayAddrs {
				if addr != node.RelayAddrs[i] {
					relayupdate = true
				}
			}
		}
	}
	relayedUpdate := false
	if node.IsRelayed == "yes" && (node.Address != newNode.Address || node.Address6 != newNode.Address6) {
		relayedUpdate = true
	}

	if !servercfg.GetRce() {
		newNode.PostDown = node.PostDown
		newNode.PostUp = node.PostUp
	}

	ifaceDelta := logic.IfaceDelta(&node, &newNode)

	err = logic.UpdateNode(&node, &newNode)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}
	if relayupdate {
		updatenodes := logic.UpdateRelay(node.Network, node.RelayAddrs, newNode.RelayAddrs)
		if err = logic.NetworkNodesUpdatePullChanges(node.Network); err != nil {
			logger.Log(1, "error setting relay updates:", err.Error())
		}
		if len(updatenodes) > 0 {
			for _, relayedNode := range updatenodes {
				runUpdates(&relayedNode, false)
			}
		}
	}
	if relayedUpdate {
		updateRelay(&node, &newNode)
	}
	if servercfg.IsDNSMode() {
		logic.SetDNS()
	}

	logger.Log(1, r.Header.Get("user"), "updated node", node.ID, "on network", node.Network)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(newNode)

	runUpdates(&newNode, ifaceDelta)
}

func deleteNode(w http.ResponseWriter, r *http.Request) {
	// Set header
	w.Header().Set("Content-Type", "application/json")

	// get params
	var params = mux.Vars(r)
	var nodeid = params["nodeid"]
	var node, err = logic.GetNodeByID(nodeid)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "badrequest"))
		return
	}
	if isServer(&node) {
		returnErrorResponse(w, r, formatError(fmt.Errorf("cannot delete server node"), "badrequest"))
		return
	}
	//send update to node to be deleted before deleting on server otherwise message cannot be sent
	node.Action = models.NODE_DELETE

	err = logic.DeleteNodeByID(&node, false)
	if err != nil {
		returnErrorResponse(w, r, formatError(err, "internal"))
		return
	}
	returnSuccessResponse(w, r, nodeid+" deleted.")

	logger.Log(1, r.Header.Get("user"), "Deleted node", nodeid, "from network", params["network"])
	runUpdates(&node, false)
	runForceServerUpdate(&node)
}

func runUpdates(node *models.Node, ifaceDelta bool) {
	go func() { // don't block http response
		// publish node update if not server
		if err := mq.NodeUpdate(node); err != nil {
			logger.Log(1, "error publishing node update to node", node.Name, node.ID, err.Error())
		}

		if err := runServerUpdate(node, ifaceDelta); err != nil {
			logger.Log(1, "error running server update", err.Error())
		}
	}()
}

// updates local peers for a server on a given node's network
func runServerUpdate(node *models.Node, ifaceDelta bool) error {

	if servercfg.IsClientMode() != "on" || !isServer(node) {
		return nil
	}

	currentServerNode, err := logic.GetNetworkServerLocal(node.Network)
	if err != nil {
		return err
	}

	if ifaceDelta && logic.IsLeader(&currentServerNode) {
		if err := mq.PublishPeerUpdate(&currentServerNode); err != nil {
			logger.Log(1, "failed to publish peer update "+err.Error())
		}
	}

	if err := logic.ServerUpdate(&currentServerNode, ifaceDelta); err != nil {
		logger.Log(1, "server node:", currentServerNode.ID, "failed update")
		return err
	}
	return nil
}

func runForceServerUpdate(node *models.Node) {
	go func() {
		if err := mq.PublishPeerUpdate(node); err != nil {
			logger.Log(1, "failed a peer update after creation of node", node.Name)
		}

		var currentServerNode, getErr = logic.GetNetworkServerLeader(node.Network)
		if getErr == nil {
			if err := logic.ServerUpdate(&currentServerNode, false); err != nil {
				logger.Log(1, "server node:", currentServerNode.ID, "failed update")
			}
		}
	}()
}

func isServer(node *models.Node) bool {
	return node.IsServer == "yes"
}

func updateRelay(oldnode, newnode *models.Node) {
	relay := logic.FindRelay(oldnode)
	newrelay := relay
	//check if node's address has been updated and if so, update the relayAddrs of the relay node with the updated address of the relayed node
	if oldnode.Address != newnode.Address {
		for i, ip := range newrelay.RelayAddrs {
			if ip == oldnode.Address {
				newrelay.RelayAddrs = append(newrelay.RelayAddrs[:i], relay.RelayAddrs[i+1:]...)
				newrelay.RelayAddrs = append(newrelay.RelayAddrs, newnode.Address)
			}
		}
	}
	//check if node's address(v6) has been updated and if so, update the relayAddrs of the relay node with the updated address(v6) of the relayed node
	if oldnode.Address6 != newnode.Address6 {
		for i, ip := range newrelay.RelayAddrs {
			if ip == oldnode.Address {
				newrelay.RelayAddrs = append(newrelay.RelayAddrs[:i], newrelay.RelayAddrs[i+1:]...)
				newrelay.RelayAddrs = append(newrelay.RelayAddrs, newnode.Address6)
			}
		}
	}
	logic.UpdateNode(relay, newrelay)
}
