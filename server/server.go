package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"

	"github.com/glycerine/blake2b" // vendor https://github.com/dchest/blake2b"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/grpclog"

	pb "github.com/glycerine/grpc-demo/streambigfile"
)

type PeerServer struct{}

func NewPeerServer() *PeerServer {
	return &PeerServer{}
}

// implement pb.PeerServer interface
func (s *PeerServer) SendFile(stream pb.Peer_SendFileServer) error {
	p("peer.Server SendFile starting!")
	var chunkCount int64
	path := ""
	var bc int64

	hasher, err := blake2b.New(nil)
	if err != nil {
		return err
	}

	var finalChecksum []byte
	var fd *os.File

	defer func() {
		if fd != nil {
			fd.Close()
		}
		p("this server.SendFile() call got %v chunks, with "+
			"final checksum '%x'", chunkCount, finalChecksum)
	}()

	firstChunkSeen := false

	for {
		nk, err := stream.Recv()
		if err == io.EOF {
			if nk != nil && len(nk.Data) > 0 {
				// we are assuming that this never happens!
				panic("we need to save this last chunk too!")
			}
			finalChecksum = []byte(hasher.Sum(nil))
			endTime := time.Now()
			return stream.SendAndClose(&pb.BigFileAck{
				Filepath:         path,
				SizeInBytes:      bc,
				RecvTime:         uint64(endTime.UnixNano()),
				WholeFileBlake2B: finalChecksum,
			})
		}
		if err != nil {
			return err
		}

		// INVAR: we have a chunk
		if !firstChunkSeen {
			if nk.Filepath != "" {
				fd, err = os.Create(nk.Filepath + fmt.Sprintf("__%v", time.Now()))
				if err != nil {
					return err
				}
			}
			firstChunkSeen = true
		}

		hasher.Write(nk.Data)
		cumul := []byte(hasher.Sum(nil))
		if 0 != bytes.Compare(cumul, nk.Blake2BCumulative) {
			return fmt.Errorf("cumulative checksums failed at chunk %v of '%s'. Observed: '%x', expected: '%x'.", nk.ChunkNumber, nk.Filepath, cumul, nk.Blake2BCumulative)
		}

		if path == "" {
			path = nk.Filepath
			p("peer.Server SendFile sees new file '%s'", path)
		}
		if path != "" && path != nk.Filepath {
			panic(fmt.Errorf("confusing between two different streams! '%s' vs '%s'", path, nk.Filepath))
		}

		if nk.SizeInBytes != int64(len(nk.Data)) {
			return fmt.Errorf("%v == nk.SizeInBytes != int64(len(nk.Data)) == %v", nk.SizeInBytes, int64(len(nk.Data)))
		}

		checksum := blake2bOfBytes(nk.Data)
		cmp := bytes.Compare(checksum, nk.Blake2B)
		if cmp != 0 {
			return fmt.Errorf("chunk %v bad .Data, checksum mismatch!", nk.ChunkNumber)
		}

		// INVAR: chunk passes tests, keep it.

		bc += nk.SizeInBytes
		chunkCount++

		// TODO: user should store chunk somewhere here... or accumulate
		// all the chunks in memory
		// until ready to store it elsewhere; e.g. in boltdb.

		// For example, we will write to the nk.Filepath, if specified.
		if fd != nil {
			rs := 0
			for {
				nw, err := fd.Write(nk.Data[rs:])
				rs += nw
				if rs == len(nk.Data) {
					// wrote it all
					break
				}
				if err == io.ErrShortWrite {
					continue
				}
				if err != nil {
					return err
				}
			}
		}
	}
	return nil
}

const ProgramName = "server"

func main() {

	myflags := flag.NewFlagSet(ProgramName, flag.ContinueOnError)
	cfg := &ServerConfig{}
	cfg.DefineFlags(myflags)
	args := os.Args[1:]
	err := myflags.Parse(args)
	_ = err
	// ignore errors so that -adduser can work, when passing os.Args[1:]
	// to the serverSshMain().

	err = cfg.ValidateConfig()
	if err != nil {
		log.Fatalf("%s command line flag error: '%s'", ProgramName, err)
	}

	var gRpcBindPort int
	var gRpcHost string
	if !cfg.Ssh {
		// use TLS
		gRpcBindPort = cfg.ExternalLsnPort
		gRpcHost = cfg.Host

		p("gRPC with TLS listening on %v:%v", gRpcHost, gRpcBindPort)

	} else {
		// SSH will take the external, gRPC will take the internal.
		gRpcBindPort = cfg.InternalLsnPort
		gRpcHost = "127.0.0.1" // local only, behind the SSHD

		p("external SSHd listening on %v:%v, internal gRPC service listening on 127.0.0.1:%v", cfg.Host, cfg.ExternalLsnPort, cfg.InternalLsnPort)

	}

	lis, err := net.Listen("tcp", fmt.Sprintf("%v:%d", gRpcHost, gRpcBindPort))
	if err != nil {
		grpclog.Fatalf("failed to listen: %v", err)
	}

	var opts []grpc.ServerOption

	if !cfg.Ssh {
		// use TLS
		creds, err := credentials.NewServerTLSFromFile(cfg.CertPath, cfg.KeyPath)
		if err != nil {
			grpclog.Fatalf("Failed to generate credentials %v", err)
		}
		opts = []grpc.ServerOption{grpc.Creds(creds)}
	} else {
		// use SSH
		args = subtractArg(args, "-ssh")
		err = serverSshMain(args, cfg.Host,
			cfg.ExternalLsnPort, cfg.InternalLsnPort)
		panicOn(err)
	}

	grpcServer := grpc.NewServer(opts...)
	pb.RegisterPeerServer(grpcServer, NewPeerServer())
	grpcServer.Serve(lis)
}

func blake2bOfBytes(by []byte) []byte {
	h, err := blake2b.New(nil)
	panicOn(err)
	h.Write(by)
	return []byte(h.Sum(nil))
}

func subtractArg(args []string, excludeMe string) (res []string) {
	for _, v := range args {
		if v != excludeMe {
			res = append(res, v)
		}
	}
	return
}
