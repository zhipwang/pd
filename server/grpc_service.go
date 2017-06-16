// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"fmt"
	"io"

	log "github.com/Sirupsen/logrus"
	"github.com/juju/errors"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// notLeaderError is returned when current server is not the leader and not possible to process request.
// TODO: work as proxy.
var notLeaderError = grpc.Errorf(codes.Unavailable, "not leader")

// GetMembers implements gRPC PDServer.
func (s *Server) GetMembers(context.Context, *pdpb.GetMembersRequest) (*pdpb.GetMembersResponse, error) {
	if s.isClosed() {
		return nil, grpc.Errorf(codes.Unknown, "server not started")
	}
	members, err := GetMembers(s.GetClient())
	if err != nil {
		return nil, grpc.Errorf(codes.Unknown, err.Error())
	}

	leader, err := s.GetLeader()
	if err != nil {
		return nil, grpc.Errorf(codes.Unknown, err.Error())
	}

	return &pdpb.GetMembersResponse{
		Header:  s.header(),
		Members: members,
		Leader:  leader,
	}, nil
}

// Tso implements gRPC PDServer.
func (s *Server) Tso(stream pdpb.PD_TsoServer) error {
	for {
		request, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.Trace(err)
		}
		if err = s.validateRequest(request.GetHeader()); err != nil {
			return errors.Trace(err)
		}
		count := request.GetCount()
		ts, err := s.getRespTS(count)
		if err != nil {
			return grpc.Errorf(codes.Unknown, err.Error())
		}
		response := &pdpb.TsoResponse{
			Header:    s.header(),
			Timestamp: &ts,
			Count:     count,
		}
		if err := stream.Send(response); err != nil {
			return errors.Trace(err)
		}
	}
}

// Bootstrap implements gRPC PDServer.
func (s *Server) Bootstrap(ctx context.Context, request *pdpb.BootstrapRequest) (*pdpb.BootstrapResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, errors.Trace(err)
	}

	cluster := s.GetRaftCluster()
	if cluster != nil {
		err := &pdpb.Error{
			Type:    pdpb.ErrorType_ALREADY_BOOTSTRAPPED,
			Message: "cluster is already bootstrapped",
		}
		return &pdpb.BootstrapResponse{
			Header: s.errorHeader(err),
		}, nil
	}
	if _, err := s.bootstrapCluster(request); err != nil {
		return nil, grpc.Errorf(codes.Unknown, err.Error())
	}

	return &pdpb.BootstrapResponse{
		Header: s.header(),
	}, nil
}

// IsBootstrapped implements gRPC PDServer.
func (s *Server) IsBootstrapped(ctx context.Context, request *pdpb.IsBootstrappedRequest) (*pdpb.IsBootstrappedResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, errors.Trace(err)
	}

	cluster := s.GetRaftCluster()
	return &pdpb.IsBootstrappedResponse{
		Header:       s.header(),
		Bootstrapped: cluster != nil,
	}, nil
}

// AllocID implements gRPC PDServer.
func (s *Server) AllocID(ctx context.Context, request *pdpb.AllocIDRequest) (*pdpb.AllocIDResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, errors.Trace(err)
	}

	// We can use an allocator for all types ID allocation.
	id, err := s.idAlloc.Alloc()
	if err != nil {
		return nil, grpc.Errorf(codes.Unknown, err.Error())
	}

	return &pdpb.AllocIDResponse{
		Header: s.header(),
		Id:     id,
	}, nil
}

// GetStore implements gRPC PDServer.
func (s *Server) GetStore(ctx context.Context, request *pdpb.GetStoreRequest) (*pdpb.GetStoreResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, errors.Trace(err)
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.GetStoreResponse{Header: s.notBootstrappedHeader()}, nil
	}

	store, _, err := cluster.GetStore(request.GetStoreId())
	if err != nil {
		return nil, grpc.Errorf(codes.Unknown, err.Error())
	}
	return &pdpb.GetStoreResponse{
		Header: s.header(),
		Store:  store,
	}, nil
}

// checkStore2 returns an error response if the store exists and is in tombstone state.
// It returns nil if it can't get the store.
// Copied from server/command.go
func checkStore2(cluster *RaftCluster, storeID uint64) *pdpb.Error {
	store, _, err := cluster.GetStore(storeID)
	if err == nil && store != nil {
		if store.GetState() == metapb.StoreState_Tombstone {
			return &pdpb.Error{
				Type:    pdpb.ErrorType_STORE_TOMBSTONE,
				Message: "store is tombstone",
			}
		}
	}
	return nil
}

// PutStore implements gRPC PDServer.
func (s *Server) PutStore(ctx context.Context, request *pdpb.PutStoreRequest) (*pdpb.PutStoreResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, errors.Trace(err)
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.PutStoreResponse{Header: s.notBootstrappedHeader()}, nil
	}

	store := request.GetStore()
	if pberr := checkStore2(cluster, store.GetId()); pberr != nil {
		return &pdpb.PutStoreResponse{
			Header: s.errorHeader(pberr),
		}, nil
	}

	if err := cluster.putStore(store); err != nil {
		return nil, grpc.Errorf(codes.Unknown, err.Error())
	}

	log.Infof("put store ok - %v", store)

	return &pdpb.PutStoreResponse{
		Header: s.header(),
	}, nil
}

// StoreHeartbeat implements gRPC PDServer.
func (s *Server) StoreHeartbeat(ctx context.Context, request *pdpb.StoreHeartbeatRequest) (*pdpb.StoreHeartbeatResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, errors.Trace(err)
	}

	if request.GetStats() == nil {
		return nil, errors.Errorf("invalid store heartbeat command, but %v", request)
	}
	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.StoreHeartbeatResponse{Header: s.notBootstrappedHeader()}, nil
	}

	if pberr := checkStore2(cluster, request.GetStats().GetStoreId()); pberr != nil {
		return &pdpb.StoreHeartbeatResponse{
			Header: s.errorHeader(pberr),
		}, nil
	}

	err := cluster.cachedCluster.handleStoreHeartbeat(request.Stats)
	if err != nil {
		return nil, grpc.Errorf(codes.Unknown, err.Error())
	}

	return &pdpb.StoreHeartbeatResponse{
		Header: s.header(),
	}, nil
}

func (s *Server) pushRegionHeartbeatResponse(stream pdpb.PD_RegionHeartbeatServer, watcher <-chan *operatorRequest, errChan chan error) {
	for {
		select {
		case <-stream.Context().Done():
			return
		case req := <-watcher:
			resp := req.resp
			resp.Header = s.header()
			err := stream.Send(resp)
			log.Info("send steam", resp, err)
			req.err <- err
			if err != nil {
				errChan <- err
				return
			}
		}
	}
}

// RegionHeartbeat implements gRPC PDServer.
func (s *Server) RegionHeartbeat(stream pdpb.PD_RegionHeartbeatServer) error {
	isNewStream := true
	errChan := make(chan error)
	for {
		select {
		case err := <-errChan:
			return errors.Trace(err)
		default:
		}
		request, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.Trace(err)
		}
		if isNewStream {
			if err = s.validateRequest(request.GetHeader()); err != nil {
				return errors.Trace(err)
			}
			storeID := request.GetLeader().GetStoreId()
			uid, err := s.idAlloc.Alloc()
			if err != nil {
				return errors.Trace(err)
			}
			watcher := s.cluster.coordinator.WatchOperator(storeID, uid)
			go s.pushRegionHeartbeatResponse(stream, watcher, errChan)
			isNewStream = false
		}

		region := newRegionInfo(request.GetRegion(), request.GetLeader())
		region.DownPeers = request.GetDownPeers()
		region.PendingPeers = request.GetPendingPeers()
		region.WrittenBytes = request.GetBytesWritten()
		cluster := s.GetRaftCluster()

		if cluster == nil {
			msg := "cluster is not bootstrapped"
			err = sendErrorRegionHeartbeatResponse(s.cluster.coordinator, region, s.clusterID, pdpb.ErrorType_UNKNOWN, msg)
			if err != nil {
				return errors.Trace(err)
			}
			continue
		}

		if region.GetId() == 0 {
			msg := fmt.Sprintf("invalid request region, %v", request)
			err = sendErrorRegionHeartbeatResponse(s.cluster.coordinator, region, s.clusterID, pdpb.ErrorType_UNKNOWN, msg)
			if err != nil {
				return errors.Trace(err)
			}
			continue
		}
		if region.Leader == nil {
			msg := fmt.Sprintf("invalid request leader, %v", request)
			err = sendErrorRegionHeartbeatResponse(s.cluster.coordinator, region, s.clusterID, pdpb.ErrorType_UNKNOWN, msg)
			if err != nil {
				return errors.Trace(err)
			}
			continue
		}

		err = cluster.cachedCluster.handleRegionHeartbeat(region)
		if err != nil {
			msg := errors.Trace(err).Error()
			err = sendErrorRegionHeartbeatResponse(s.cluster.coordinator, region, s.clusterID, pdpb.ErrorType_UNKNOWN, msg)
			if err != nil {
				return errors.Trace(err)
			}
			continue
		}

		resp, err := cluster.handleRegionHeartbeat(region)
		if err != nil {
			msg := errors.Trace(err).Error()
			err = sendErrorRegionHeartbeatResponse(s.cluster.coordinator, region, s.clusterID, pdpb.ErrorType_UNKNOWN, msg)
			if err != nil {
				return errors.Trace(err)
			}
			continue
		}
		if resp == nil {
			if s.cfg.regionHeartbeatUnaryMode {
				// A workaround for passing tests, remove it ASAP.
				resp = new(pdpb.RegionHeartbeatResponse)
				s.cluster.coordinator.sendToWatcher(region, resp)
			} else {
				// No operations, skip.
				continue
			}
		}

	}
}

// GetRegion implements gRPC PDServer.
func (s *Server) GetRegion(ctx context.Context, request *pdpb.GetRegionRequest) (*pdpb.GetRegionResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, errors.Trace(err)
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.GetRegionResponse{Header: s.notBootstrappedHeader()}, nil
	}
	region, leader := cluster.GetRegionByKey(request.GetRegionKey())
	log.Info("Debug", region, leader, request.GetRegionKey())
	return &pdpb.GetRegionResponse{
		Header: s.header(),
		Region: region,
		Leader: leader,
	}, nil
}

// GetRegionByID implements gRPC PDServer.
func (s *Server) GetRegionByID(ctx context.Context, request *pdpb.GetRegionByIDRequest) (*pdpb.GetRegionResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, errors.Trace(err)
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.GetRegionResponse{Header: s.notBootstrappedHeader()}, nil
	}
	id := request.GetRegionId()
	region, leader := cluster.GetRegionByID(id)
	return &pdpb.GetRegionResponse{
		Header: s.header(),
		Region: region,
		Leader: leader,
	}, nil
}

// AskSplit implements gRPC PDServer.
func (s *Server) AskSplit(ctx context.Context, request *pdpb.AskSplitRequest) (*pdpb.AskSplitResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, errors.Trace(err)
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.AskSplitResponse{Header: s.notBootstrappedHeader()}, nil
	}
	if request.GetRegion().GetStartKey() == nil {
		return nil, errors.New("missing region start key for split")
	}
	req := &pdpb.AskSplitRequest{
		Region: request.Region,
	}
	split, err := cluster.handleAskSplit(req)
	if err != nil {
		return nil, grpc.Errorf(codes.Unknown, err.Error())
	}

	return &pdpb.AskSplitResponse{
		Header:      s.header(),
		NewRegionId: split.NewRegionId,
		NewPeerIds:  split.NewPeerIds,
	}, nil
}

// ReportSplit implements gRPC PDServer.
func (s *Server) ReportSplit(ctx context.Context, request *pdpb.ReportSplitRequest) (*pdpb.ReportSplitResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, errors.Trace(err)
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.ReportSplitResponse{Header: s.notBootstrappedHeader()}, nil
	}
	_, err := cluster.handleReportSplit(request)
	if err != nil {
		return nil, grpc.Errorf(codes.Unknown, err.Error())
	}

	return &pdpb.ReportSplitResponse{
		Header: s.header(),
	}, nil
}

// GetClusterConfig implements gRPC PDServer.
func (s *Server) GetClusterConfig(ctx context.Context, request *pdpb.GetClusterConfigRequest) (*pdpb.GetClusterConfigResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, errors.Trace(err)
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.GetClusterConfigResponse{Header: s.notBootstrappedHeader()}, nil
	}
	return &pdpb.GetClusterConfigResponse{
		Header:  s.header(),
		Cluster: cluster.GetConfig(),
	}, nil
}

// PutClusterConfig implements gRPC PDServer.
func (s *Server) PutClusterConfig(ctx context.Context, request *pdpb.PutClusterConfigRequest) (*pdpb.PutClusterConfigResponse, error) {
	if err := s.validateRequest(request.GetHeader()); err != nil {
		return nil, errors.Trace(err)
	}

	cluster := s.GetRaftCluster()
	if cluster == nil {
		return &pdpb.PutClusterConfigResponse{Header: s.notBootstrappedHeader()}, nil
	}
	conf := request.GetCluster()
	if err := cluster.putConfig(conf); err != nil {
		return nil, grpc.Errorf(codes.Unknown, err.Error())
	}

	log.Infof("put cluster config ok - %v", conf)

	return &pdpb.PutClusterConfigResponse{
		Header: s.header(),
	}, nil
}

// validateRequest checks if Server is leader and clusterID is matched.
// TODO: Call it in gRPC intercepter.
func (s *Server) validateRequest(header *pdpb.RequestHeader) error {
	if !s.IsLeader() {
		return notLeaderError
	}
	if header.GetClusterId() != s.clusterID {
		return grpc.Errorf(codes.FailedPrecondition, "mismatch cluster id, need %d but got %d", s.clusterID, header.GetClusterId())
	}
	return nil
}

func (s *Server) header() *pdpb.ResponseHeader {
	return &pdpb.ResponseHeader{ClusterId: s.clusterID}
}

func (s *Server) errorHeader(err *pdpb.Error) *pdpb.ResponseHeader {
	return &pdpb.ResponseHeader{
		ClusterId: s.clusterID,
		Error:     err,
	}
}

func (s *Server) notBootstrappedHeader() *pdpb.ResponseHeader {
	return s.errorHeader(&pdpb.Error{
		Type:    pdpb.ErrorType_NOT_BOOTSTRAPPED,
		Message: "cluster is not bootstrapped",
	})
}

func sendErrorRegionHeartbeatResponse(c *coordinator, region *RegionInfo, clusterID uint64, ty pdpb.ErrorType, msg string) error {
	pberr := &pdpb.Error{
		Type:    ty,
		Message: msg,
	}
	resp := &pdpb.RegionHeartbeatResponse{}
	resp.Header = &pdpb.ResponseHeader{
		ClusterId: clusterID,
		Error:     pberr,
	}
	c.sendToWatcher(region, resp)
	return nil
}
