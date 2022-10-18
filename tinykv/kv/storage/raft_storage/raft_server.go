package raft_storage

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"github.com/pingcap-incubator/tinykv/kv/config"
	"github.com/pingcap-incubator/tinykv/kv/raftstore"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/message"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/scheduler_client"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/snap"
	"github.com/pingcap-incubator/tinykv/kv/storage"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
	"github.com/pingcap-incubator/tinykv/kv/util/worker"
	"github.com/pingcap-incubator/tinykv/proto/pkg/errorpb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/kvrpcpb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/raft_cmdpb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/tinykvpb"
	"github.com/pingcap/errors"
)

// RaftStorage is an implementation of `Storage` (see tikv/server.go) backed by a Raft node. It is part of a Raft network.
// By using Raft, reads and writes are consistent with other nodes in the TinyKV instance.
type RaftStorage struct {
	engines *engine_util.Engines
	config  *config.Config

	node          *raftstore.Node
	snapManager   *snap.SnapManager
	raftRouter    *raftstore.RaftstoreRouter
	raftSystem    *raftstore.Raftstore
	resolveWorker *worker.Worker
	snapWorker    *worker.Worker

	wg     sync.WaitGroup
	client scheduler_client.Client
}

type RegionError struct {
	RequestErr *errorpb.Error
}

func (re *RegionError) Error() string {
	return re.RequestErr.String()
}

func (rs *RaftStorage) checkResponse(resp *raft_cmdpb.RaftCmdResponse, reqCount int) error {
	if resp.Header.Error != nil {
		return &RegionError{RequestErr: resp.Header.Error}
	}
	if len(resp.Responses) != reqCount {
		return errors.Errorf("responses count %d is not equal to requests count %d",
			len(resp.Responses), reqCount)
	}
	return nil
}

// NewRaftStorage creates a new storage engine backed by a raftstore.
func NewRaftStorage(conf *config.Config) *RaftStorage {
	dbPath := conf.DBPath
	kvPath := filepath.Join(dbPath, "kv")
	raftPath := filepath.Join(dbPath, "raft")
	snapPath := filepath.Join(dbPath, "snap")

	os.MkdirAll(kvPath, os.ModePerm)
	os.MkdirAll(raftPath, os.ModePerm)
	os.Mkdir(snapPath, os.ModePerm)

	raftDB := engine_util.CreateDB("raft", conf)
	kvDB := engine_util.CreateDB("kv", conf)
	engines := engine_util.NewEngines(kvDB, raftDB, kvPath, raftPath)

	return &RaftStorage{engines: engines, config: conf}
}

// Write is the main entrance for the upper layer, it's used to submit the write requests
// to the raft store layer. All the modifies will be transformed into raft store request
// types, these raft store requests will be sent to the raft router and the raft worker will
// process these raft store requests. When these request are processed, say the write log has
// been persisted on the majority of the raft group and applied by the state machine, the callback
// will wakeup the upper layer to send response to the kv client.
func (rs *RaftStorage) Write(ctx *kvrpcpb.Context, batch []storage.Modify) error {
	var reqs []*raft_cmdpb.Request
	for _, m := range batch {
		switch m.Data.(type) {
		case storage.Put:
			put := m.Data.(storage.Put)
			reqs = append(reqs, &raft_cmdpb.Request{
				CmdType: raft_cmdpb.CmdType_Put,
				Put: &raft_cmdpb.PutRequest{
					Cf:    put.Cf,
					Key:   put.Key,
					Value: put.Value,
				}})
		case storage.Delete:
			delete := m.Data.(storage.Delete)
			reqs = append(reqs, &raft_cmdpb.Request{
				CmdType: raft_cmdpb.CmdType_Delete,
				Delete: &raft_cmdpb.DeleteRequest{
					Cf:  delete.Cf,
					Key: delete.Key,
				}})
		}
	}

	header := &raft_cmdpb.RaftRequestHeader{
		RegionId:    ctx.RegionId,
		Peer:        ctx.Peer,
		RegionEpoch: ctx.RegionEpoch,
		Term:        ctx.Term,
	}
	request := &raft_cmdpb.RaftCmdRequest{
		Header:   header,
		Requests: reqs,
	}
	cb := message.NewCallback()
	if err := rs.raftRouter.SendRaftCommand(request, cb); err != nil {
		return err
	}

	return rs.checkResponse(cb.WaitResp(), len(reqs))
}

// Reader is main entrance to get a snapshot of current state machine for read. Only
// the raft group or region leader could process read requests, to ensure this another
// raft instance is used and the snapshot is generated by the applier. There are also
// better ways to reduce the cost of read request processing, more information about
// this could be found in the raft paper 6.4.
func (rs *RaftStorage) Reader(ctx *kvrpcpb.Context) (storage.StorageReader, error) {
	header := &raft_cmdpb.RaftRequestHeader{
		RegionId:    ctx.RegionId,
		Peer:        ctx.Peer,
		RegionEpoch: ctx.RegionEpoch,
		Term:        ctx.Term,
	}
	request := &raft_cmdpb.RaftCmdRequest{
		Header: header,
		Requests: []*raft_cmdpb.Request{{
			CmdType: raft_cmdpb.CmdType_Snap,
			Snap:    &raft_cmdpb.SnapRequest{},
		}},
	}
	cb := message.NewCallback()
	if err := rs.raftRouter.SendRaftCommand(request, cb); err != nil {
		return nil, err
	}

	resp := cb.WaitResp()
	if err := rs.checkResponse(resp, 1); err != nil {
		if cb.Txn != nil {
			cb.Txn.Discard()
		}
		return nil, err
	}
	if cb.Txn == nil {
		panic("can not found region snap")
	}
	if len(resp.Responses) != 1 {
		panic("wrong response count for snap cmd")
	}
	return NewRegionReader(cb.Txn, *resp.Responses[0].GetSnap().Region), nil
}

func (rs *RaftStorage) Raft(stream tinykvpb.TinyKv_RaftServer) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		rs.raftRouter.SendRaftMessage(msg)
	}
}

func (rs *RaftStorage) Snapshot(stream tinykvpb.TinyKv_SnapshotServer) error {
	var err error
	done := make(chan struct{})
	rs.snapWorker.Sender() <- &recvSnapTask{
		stream: stream,
		callback: func(e error) {
			err = e
			close(done)
		},
	}
	<-done
	return err
}

// Start the raftStore and prepare for process requests.
func (rs *RaftStorage) Start(client scheduler_client.Client) error {
	rs.client = client
	cfg := rs.config
	rs.raftRouter, rs.raftSystem = raftstore.CreateRaftstore(cfg)

	rs.resolveWorker = worker.NewWorker("resolver", &rs.wg)
	resolveSender := rs.resolveWorker.Sender()
	resolveRunner := newResolverRunner(client)
	rs.resolveWorker.Start(resolveRunner)

	rs.snapManager = snap.NewSnapManager(filepath.Join(cfg.DBPath, "snap"))
	rs.snapWorker = worker.NewWorker("snap-worker", &rs.wg)
	snapSender := rs.snapWorker.Sender()
	snapRunner := newSnapRunner(rs.snapManager, rs.config, rs.raftRouter)
	rs.snapWorker.Start(snapRunner)

	raftClient := newRaftClient(cfg)
	trans := NewServerTransport(raftClient, snapSender, rs.raftRouter, resolveSender)

	rs.node = raftstore.NewNode(rs.raftSystem, rs.config, client)
	err := rs.node.Start(context.TODO(), rs.engines, trans, rs.snapManager)
	if err != nil {
		return err
	}

	return nil
}

// Stop the raft store.
func (rs *RaftStorage) Stop() error {
	rs.snapWorker.Stop()
	rs.node.Stop()
	rs.resolveWorker.Stop()
	rs.wg.Wait()
	if err := rs.engines.Raft.Close(); err != nil {
		return err
	}
	if err := rs.engines.Kv.Close(); err != nil {
		return err
	}
	return nil
}

func (rs *RaftStorage) Client() scheduler_client.Client {
	return rs.client
}