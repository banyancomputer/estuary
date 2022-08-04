package util

import (
	"errors"
	"fmt"
	"io"

	"github.com/ipfs/go-cidutil"
	chunker "github.com/ipfs/go-ipfs-chunker"
	ipld "github.com/ipfs/go-ipld-format"
	"github.com/ipfs/go-merkledag"
	unixfs "github.com/ipfs/go-unixfs"
	"github.com/ipfs/go-unixfs/importer/balanced"
	ihelper "github.com/ipfs/go-unixfs/importer/helpers"
	mh "github.com/multiformats/go-multihash"

	/*Banyan Oracle Storage */
	banyan_oracle "github.com/banyancomputer/oracle-storage"
)

var DefaultHashFunction = uint64(mh.SHA2_256)

func ImportFile(dserv ipld.DAGService, fi io.Reader, filename string, filesize int64) (ipld.Node, error) {
	prefix, err := merkledag.PrefixForCidVersion(1)
	if err != nil {
		return nil, err
	}
	prefix.MhType = DefaultHashFunction

	spl := chunker.NewSizeSplitter(fi, 1024*1024)
	dbp := ihelper.DagBuilderParams{
		Maxlinks:  1024,
		RawLeaves: true,

		CidBuilder: cidutil.InlineBuilder{
			Builder: prefix,
			Limit:   32,
		},

		Dagserv: dserv,
	}

	db, err := dbp.New(spl)
	if err != nil {
		return nil, err
	}

	ret, err := balanced.Layout(db)
    if err != nil {
        return nil, err
    }


    err = banyan_oracle.Store(filename, filesize, ret.Cid().String());
    println("Storing file in banyan_oracle: " + filename + " " + ret.Cid().String());
    if err != nil {
        println("Error storing file in banyan_oracle: " + filename + " " + ret.Cid().String());
        println(err.Error());
        return nil, err
    }

    return ret, nil
}

func TryExtractFSNode(nd ipld.Node) (*unixfs.FSNode, error) {
	switch nd := nd.(type) {
	case *merkledag.ProtoNode:
		n, err := unixfs.FSNodeFromBytes(nd.Data())
		if err != nil {
			return nil, err
		}
		if n.Type() == unixfs.TSymlink {
			return nil, fmt.Errorf("symlinks not supported")
		}
		return n, nil // success!
	case *merkledag.RawNode:
	default:
		return nil, errors.New("unknown node type")
	}
	return nil, errors.New("unknown node type")
}
