package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/filecoin-project/go-address"
	cborutil "github.com/filecoin-project/go-cbor-util"
	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/filecoin-project/go-fil-markets/storagemarket"
	"github.com/filecoin-project/go-fil-markets/storagemarket/network"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/specs-actors/actors/builtin/market"
	"github.com/ipfs/go-cid"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/whyrusleeping/estuary/filclient"
	"golang.org/x/xerrors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ContentManager struct {
	DB        *gorm.DB
	Api       api.Gateway
	FilClient *filclient.FilClient

	Blockstore blockstore.Blockstore
	Tracker    *TrackingBlockstore

	ToCheck chan uint
}

func NewContentManager(db *gorm.DB, api api.Gateway, fc *filclient.FilClient, tbs *TrackingBlockstore) *ContentManager {
	return &ContentManager{
		DB:         db,
		Api:        api,
		FilClient:  fc,
		Blockstore: tbs.Under(),
		Tracker:    tbs,
		ToCheck:    make(chan uint, 10),
	}
}

func (cm *ContentManager) ContentWatcher() {
	if err := cm.startup(); err != nil {
		log.Errorf("failed to recheck existing content: %s", err)
	}

	ticker := time.Tick(time.Minute * 5)

	for {
		select {
		case c := <-cm.ToCheck:
			var content Content
			if err := cm.DB.First(&content, "id = ?", c).Error; err != nil {
				log.Errorf("finding content %d in database: %s", c, err)
				continue
			}

			if err := cm.ensureStorage(context.TODO(), content); err != nil {
				log.Errorf("failed to ensure replication of content %d: %s", content.ID, err)
				continue
			}
		case <-ticker:
			if err := cm.queueAllContent(); err != nil {
				log.Errorf("rechecking content: %s", err)
				continue
			}
		}
	}
}

func (cm *ContentManager) startup() error {
	return nil
	// TODO: something a wee bit smarter
	return cm.queueAllContent()
}

func (cm *ContentManager) queueAllContent() error {
	var allcontent []Content
	if err := cm.DB.Find(&allcontent, "active").Error; err != nil {
		return xerrors.Errorf("finding all content in database: %w", err)
	}

	go func() {
		for _, c := range allcontent {
			cm.ToCheck <- c.ID
		}
	}()

	return nil
}

func (cm *ContentManager) estimatePrice(ctx context.Context, repl int, size abi.PaddedPieceSize, duration abi.ChainEpoch, verified bool) (*abi.TokenAmount, error) {
	miners, err := cm.pickMiners(ctx, repl, size)
	if err != nil {
		return nil, err
	}
	if len(miners) == 0 {
		return nil, fmt.Errorf("failed to find any miners for estimating deal price")
	}

	fmt.Println("miners: ", miners)
	total := abi.NewTokenAmount(0)
	for _, m := range miners {
		ask, err := cm.getAsk(ctx, m, time.Minute*30)
		if err != nil {
			return nil, err
		}

		var price *abi.TokenAmount
		if verified {
			p, err := ask.GetVerifiedPrice()
			if err != nil {
				return nil, err
			}
			price = p
		} else {
			p, err := ask.GetPrice()
			if err != nil {
				return nil, err
			}
			price = p
		}

		fmt.Println("ask price: ", price)

		cost, err := filclient.ComputePrice(*price, size, duration)
		if err != nil {
			return nil, err
		}

		total = types.BigAdd(total, *cost)
	}

	return &total, nil
}

type minerStorageAsk struct {
	gorm.Model
	Miner         string `gorm:"unique"`
	Price         string
	VerifiedPrice string
	MinPieceSize  abi.PaddedPieceSize
}

func (msa *minerStorageAsk) GetPrice() (*types.BigInt, error) {
	v, err := types.BigFromString(msa.Price)
	if err != nil {
		return nil, err
	}

	return &v, nil
}

func (msa *minerStorageAsk) GetVerifiedPrice() (*types.BigInt, error) {
	v, err := types.BigFromString(msa.VerifiedPrice)
	if err != nil {
		return nil, err
	}

	return &v, nil
}

func (cm *ContentManager) pickMiners(ctx context.Context, n int, size abi.PaddedPieceSize) ([]address.Address, error) {
	var dbminers []storageMiner
	if err := cm.DB.Order("random()").Find(&dbminers).Error; err != nil {
		return nil, err
	}
	var out []address.Address
	for _, val := range dbminers {
		m := val.Address.Addr
		ask, err := cm.getAsk(ctx, m, time.Minute*30)
		if err != nil {
			log.Errorf("getting ask from %s failed: %s", m, err)
			continue
		}

		fmt.Println("got ask", m, ask.Price, ask.MinPieceSize)

		if cm.sizeIsCloseEnough(size, ask.MinPieceSize) {
			out = append(out, m)
		}

		if len(out) >= n {
			fmt.Println("breaking", len(out), n)
			break
		}
	}
	return out, nil
}

func (cm *ContentManager) getAsk(ctx context.Context, m address.Address, maxCacheAge time.Duration) (*minerStorageAsk, error) {
	var msa minerStorageAsk
	if err := cm.DB.First(&msa, "miner = ?", m.String()).Error; err != nil {
		if !xerrors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}

	fmt.Println("time since update: ", time.Since(msa.UpdatedAt))
	if time.Since(msa.UpdatedAt) < maxCacheAge {
		fmt.Println("using cache!")
		return &msa, nil
	}

	netask, err := cm.FilClient.GetAsk(ctx, m)
	if err != nil {
		cm.recordDealFailure(&DealFailureError{
			Miner:   m,
			Phase:   "query-ask",
			Message: err.Error(),
		})
		return nil, err
	}

	nmsa := toDBAsk(netask)

	if err := cm.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "miner"},
		},
		DoUpdates: clause.AssignmentColumns([]string{"price", "verified_price", "min_piece_size"}),
	}).Create(nmsa).Error; err != nil {
		return nil, err
	}

	return nmsa, nil
}

func (cm *ContentManager) sizeIsCloseEnough(fsize, limit abi.PaddedPieceSize) bool {
	if fsize > limit {
		return true
	}

	if fsize*2 > limit {
		return true
	}

	return false
}

func toDBAsk(netask *network.AskResponse) *minerStorageAsk {
	return &minerStorageAsk{
		Miner:         netask.Ask.Ask.Miner.String(),
		Price:         netask.Ask.Ask.Price.String(),
		VerifiedPrice: netask.Ask.Ask.VerifiedPrice.String(),
		MinPieceSize:  netask.Ask.Ask.MinPieceSize,
	}
}

type contentDeal struct {
	gorm.Model
	Content  uint
	PropCid  dbCID
	Miner    string
	DealID   int64
	Failed   bool
	FailedAt time.Time
}

func (cd contentDeal) MinerAddr() (address.Address, error) {
	return address.NewFromString(cd.Miner)
}

func (cm *ContentManager) ensureStorage(ctx context.Context, content Content) error {

	// check if content has enough deals made for it
	// if not enough deals, go make more
	// check all existing deals, ensure they are still active
	// if not active, repair!

	var deals []contentDeal
	if err := cm.DB.Find(&deals, "content = ? AND NOT failed", content.ID).Error; err != nil {
		if !xerrors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}

	replicationFactor := 10

	if len(deals) < replicationFactor {
		// make some more deals!
		fmt.Printf("Content only has %d deals, making %d more.\n", len(deals), replicationFactor-len(deals))
		if err := cm.makeDealsForContent(ctx, content, replicationFactor-len(deals)); err != nil {
			return err
		}
	}

	// check on each of the existing deals, see if they need fixing
	for _, d := range deals {
		ok, err := cm.checkDeal(&d)
		if err != nil {
			var dfe *DealFailureError
			if xerrors.As(err, &dfe) {
				cm.recordDealFailure(dfe)
				continue
			} else {
				return err
			}
		}

		if !ok {
			if err := cm.repairDeal(&d); err != nil {
				return xerrors.Errorf("repairing deal failed: %w", err)
			}
		}
	}

	return nil
}

func (cm *ContentManager) getContent(id uint) (*Content, error) {
	var content Content
	if err := cm.DB.First(&content, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &content, nil
}

func (cm *ContentManager) checkDeal(d *contentDeal) (bool, error) {
	log.Infow("checking deal", "miner", d.Miner, "content", d.Content, "dbid", d.ID)
	ctx := context.TODO()

	maddr, err := d.MinerAddr()
	if err != nil {
		return false, err
	}

	if d.DealID != 0 {
		return cm.FilClient.CheckChainDeal(ctx, abi.DealID(d.DealID))
	}

	// case where deal isnt yet on chain...

	fmt.Println("checking proposal cid: ", d.PropCid.CID)
	provds, err := cm.FilClient.DealStatus(ctx, maddr, d.PropCid.CID)
	if err != nil {
		// if we cant get deal status from a miner and the data hasnt landed on
		// chain what do we do?
		cm.recordDealFailure(&DealFailureError{
			Miner:   maddr,
			Phase:   "check-status",
			Message: err.Error(),
			Content: d.Content,
		})
		return false, nil
	}

	content, err := cm.getContent(d.Content)
	if err != nil {
		return false, err
	}

	head, err := cm.Api.ChainHead(ctx)
	if err != nil {
		return false, err
	}

	if provds.PublishCid != nil {
		id, err := cm.getDealID(ctx, *provds.PublishCid, d)
		if err != nil {
			if xerrors.Is(err, ErrNotOnChainYet) {
				if provds.Proposal.StartEpoch < head.Height() {
					// deal expired, miner didnt start it in time
					cm.recordDealFailure(&DealFailureError{
						Miner:   maddr,
						Phase:   "check-status",
						Message: "deal did not make it on chain in time (but has publish deal cid set)",
						Content: d.Content,
					})
					return false, nil
				}
				return true, nil
			}
			return false, xerrors.Errorf("failed to check deal id: %w", err)
		}

		fmt.Println("Got deal ID for deal!", id)
		d.DealID = int64(id)
		if err := cm.DB.Save(&d).Error; err != nil {
			return false, xerrors.Errorf("failed to update database entry: %w", err)
		}
		return true, nil
	}

	if provds.Proposal.StartEpoch < head.Height() {
		// deal expired, miner didnt start it in time
		cm.recordDealFailure(&DealFailureError{
			Miner:   maddr,
			Phase:   "check-status",
			Message: "deal did not make it on chain in time",
			Content: d.Content,
		})
		return false, nil
	}
	// miner still has time...

	fmt.Println("Checking transfer status...")
	status, err := cm.FilClient.TransferStatusForContent(ctx, content.Cid.CID, maddr)
	if err != nil {
		if err != filclient.ErrNoTransferFound {
			return false, err
		}

		// no transfer found for this pair, need to create a new one
	}

	switch status.Status {
	case datatransfer.Failed:
		fmt.Println("Transfer failed!", status.Message)
		cm.recordDealFailure(&DealFailureError{
			Miner:   maddr,
			Phase:   "data-transfer",
			Message: fmt.Sprintf("transfer failed: %s", status.Message),
			Content: content.ID,
		})
	case datatransfer.Cancelled:
		fmt.Println("Transfer canceled!", status.Message)
		cm.recordDealFailure(&DealFailureError{
			Miner:   maddr,
			Phase:   "data-transfer",
			Message: fmt.Sprintf("transfer cancelled: %s", status.Message),
			Content: content.ID,
		})
	case datatransfer.Requested:
		fmt.Println("transfer is requested, hasnt started yet")
		// probably okay
	case datatransfer.TransferFinished, datatransfer.Finalizing, datatransfer.Completing, datatransfer.Completed:
		// these are all okay
		fmt.Println("transfer is finished-ish!", status.Status)
	case datatransfer.Ongoing:
		fmt.Println("transfer status is ongoing!")
		if err := cm.FilClient.CheckOngoingTransfer(ctx, maddr, status); err != nil {
			cm.recordDealFailure(&DealFailureError{
				Miner:   maddr,
				Phase:   "data-transfer",
				Message: fmt.Sprintf("error while checking transfer: %s", err),
				Content: content.ID,
			})
			return false, nil // TODO: returning false here feels excessive
		}
		// expected, this is fine
	default:
		fmt.Printf("Unexpected data transfer state: %d (msg = %s)\n", status.Status, status.Message)
	}

	return true, nil
}

var ErrNotOnChainYet = fmt.Errorf("message not found on chain")

func (cm *ContentManager) getDealID(ctx context.Context, pubcid cid.Cid, d *contentDeal) (abi.DealID, error) {
	mlookup, err := cm.Api.StateSearchMsg(ctx, pubcid)
	if err != nil {
		return 0, xerrors.Errorf("could not find published deal on chain: %w", err)
	}

	if mlookup == nil {
		return 0, ErrNotOnChainYet
	}

	if mlookup.Message != pubcid {
		// TODO: can probably deal with this by checking the message contents?
		return 0, xerrors.Errorf("publish deal message was replaced on chain")
	}

	msg, err := cm.Api.ChainGetMessage(ctx, mlookup.Message)
	if err != nil {
		return 0, err
	}

	var params market.PublishStorageDealsParams
	if err := params.UnmarshalCBOR(bytes.NewReader(msg.Params)); err != nil {
		return 0, err
	}

	dealix := -1
	for i, pd := range params.Deals {
		nd, err := cborutil.AsIpld(&pd)
		if err != nil {
			return 0, xerrors.Errorf("failed to compute deal proposal ipld node: %w", err)
		}

		if nd.Cid() == d.PropCid.CID {
			dealix = i
			break
		}
	}

	if dealix == -1 {
		return 0, fmt.Errorf("our deal was not in this publish message")
	}

	if mlookup.Receipt.ExitCode != 0 {
		return 0, xerrors.Errorf("miners deal publish failed (exit: %d)", mlookup.Receipt.ExitCode)
	}

	var retval market.PublishStorageDealsReturn
	if err := retval.UnmarshalCBOR(bytes.NewReader(mlookup.Receipt.Return)); err != nil {
		return 0, xerrors.Errorf("publish deal return was improperly formatted: %w", err)
	}

	if len(retval.IDs) != len(params.Deals) {
		return 0, fmt.Errorf("return value from publish deals did not match length of params")
	}

	return retval.IDs[dealix], nil
}

func (cm *ContentManager) repairDeal(d *contentDeal) error {
	fmt.Println("repair deal: ", d.PropCid.CID, d.Miner, d.Content)
	d.Failed = true
	d.FailedAt = time.Now()
	if err := cm.DB.Save(d).Error; err != nil {
		return err
	}

	return nil
}

var priceMax abi.TokenAmount

func init() {
	max, err := types.ParseFIL("0.00000003")
	if err != nil {
		panic(err)
	}
	priceMax = abi.TokenAmount(max)
}

func (cm *ContentManager) priceIsTooHigh(price abi.TokenAmount) bool {
	if types.BigCmp(price, priceMax) > 0 {
		return true
	}

	return false
}

type proposalRecord struct {
	PropCid dbCID
	Data    []byte
}

func (cm *ContentManager) makeDealsForContent(ctx context.Context, content Content, count int) error {

	sealType := abi.RegisteredSealProof_StackedDrg32GiBV1_1 // pull from miner...
	_, size, err := cm.getPieceCommitment(sealType, content.Cid.CID, cm.Blockstore)
	if err != nil {
		return err
	}

	minerpool, err := cm.pickMiners(ctx, count*2, size.Padded())
	if err != nil {
		return err
	}

	var asks []*network.AskResponse
	var ms []address.Address
	var successes int
	for _, m := range minerpool {
		ask, err := cm.FilClient.GetAsk(ctx, m)
		if err != nil {
			cm.recordDealFailure(&DealFailureError{
				Miner:   m,
				Phase:   "query-ask",
				Message: err.Error(),
				Content: content.ID,
			})
			fmt.Printf("failed to get ask for miner %s: %s\n", m, err)
			continue
		}

		if ask.Ask.Ask.MinPieceSize > size.Padded() {
			continue
		}

		if cm.priceIsTooHigh(ask.Ask.Ask.Price) {
			log.Infow("miners price is too high", "miner", m, "price", ask.Ask.Ask.Price)
			cm.recordDealFailure(&DealFailureError{
				Miner:   m,
				Phase:   "miner-search",
				Message: fmt.Sprintf("miners price is too high: %s", types.FIL(ask.Ask.Ask.Price)),
				Content: content.ID,
			})
			continue
		}

		ms = append(ms, m)
		asks = append(asks, ask)
		successes++
		if len(ms) >= count {
			break
		}
	}

	proposals := make([]*network.Proposal, len(ms))
	for i, m := range ms {
		if asks[i] == nil {
			continue
		}

		prop, err := cm.FilClient.MakeDeal(ctx, m, content.Cid.CID, asks[i].Ask.Ask.Price, 1000000)
		if err != nil {
			return xerrors.Errorf("failed to construct a deal proposal: %w", err)
		}

		proposals[i] = prop

		nd, err := cborutil.AsIpld(prop)
		if err != nil {
			return err
		}
		fmt.Println("proposal cid: ", nd.Cid())

		if err := cm.DB.Create(&proposalRecord{
			PropCid: dbCID{nd.Cid()},
			Data:    nd.RawData(),
		}).Error; err != nil {
			return err
		}
	}

	responses := make([]*network.SignedResponse, len(ms))
	for i, p := range proposals {
		if p == nil {
			continue
		}

		dealresp, err := cm.FilClient.SendProposal(ctx, p)
		if err != nil {
			cm.recordDealFailure(&DealFailureError{
				Miner:   ms[i],
				Phase:   "send-proposal",
				Message: err.Error(),
				Content: content.ID,
			})
			fmt.Println("failed to propose deal with miner: ", err)
			continue
		}

		// TODO: verify signature!
		switch dealresp.Response.State {
		case storagemarket.StorageDealError:
			if err := cm.recordDealFailure(&DealFailureError{
				Miner:   ms[i],
				Phase:   "propose",
				Message: dealresp.Response.Message,
				Content: content.ID,
			}); err != nil {
				return err
			}
		case storagemarket.StorageDealProposalRejected:
			if err := cm.recordDealFailure(&DealFailureError{
				Miner:   ms[i],
				Phase:   "propose",
				Message: dealresp.Response.Message,
				Content: content.ID,
			}); err != nil {
				return err
			}
		default:
			if err := cm.recordDealFailure(&DealFailureError{
				Miner:   ms[i],
				Phase:   "propose",
				Message: fmt.Sprintf("unrecognized response state %d: %s", dealresp.Response.State, dealresp.Response.Message),
				Content: content.ID,
			}); err != nil {
				return err
			}
		case storagemarket.StorageDealWaitingForData, storagemarket.StorageDealProposalAccepted:
			fmt.Println("good deal state!", dealresp.Response.State)
			responses[i] = dealresp

			cd := &contentDeal{
				Content: content.ID,
				PropCid: dbCID{dealresp.Response.Proposal},
				Miner:   ms[i].String(),
			}

			if err := cm.DB.Create(cd).Error; err != nil {
				return xerrors.Errorf("failed to create database entry for deal: %w", err)
			}
		}
	}

	// Now start up some data transfers!
	// note: its okay if we dont start all the data transfers, we can just do it next time around
	for i, resp := range responses {
		if resp == nil {
			continue
		}

		chanid, err := cm.FilClient.StartDataTransfer(ctx, ms[i], resp.Response.Proposal, content.Cid.CID)
		if err != nil {
			if oerr := cm.recordDealFailure(&DealFailureError{
				Miner:   ms[i],
				Phase:   "start-data-transfer",
				Message: err.Error(),
				Content: content.ID,
			}); oerr != nil {
				return oerr
			}
		}

		log.Infow("Started data transfer", "chanid", chanid)
	}

	return nil
}

func (cm *ContentManager) recordDealFailure(dfe *DealFailureError) error {
	log.Infow("deal failure error", "miner", dfe.Miner, "phase", dfe.Phase, "msg", dfe.Message, "content", dfe.Content)
	return cm.DB.Create(dfe.Record()).Error
}

type DealFailureError struct {
	Miner   address.Address
	Phase   string
	Message string
	Content uint
}

type dfeRecord struct {
	gorm.Model
	Miner   string
	Phase   string
	Message string
	Content uint
}

func (dfe *DealFailureError) Record() *dfeRecord {
	return &dfeRecord{
		Miner:   dfe.Miner.String(),
		Phase:   dfe.Phase,
		Message: dfe.Message,
		Content: dfe.Content,
	}
}

func (dfe *DealFailureError) Error() string {
	return fmt.Sprintf("deal with miner %s failed in phase %s: %s", dfe.Message, dfe.Phase, dfe.Message)
}

func averageAskPrice(asks []*network.AskResponse) types.FIL {
	total := abi.NewTokenAmount(0)
	for _, a := range asks {
		total = types.BigAdd(total, a.Ask.Ask.Price)
	}

	return types.FIL(big.Div(total, big.NewInt(int64(len(asks)))))
}

type PieceCommRecord struct {
	Data  dbCID `gorm:"unique"`
	Piece dbCID
	Size  abi.UnpaddedPieceSize
}

func (cm *ContentManager) getPieceCommitment(rt abi.RegisteredSealProof, data cid.Cid, bs blockstore.Blockstore) (cid.Cid, abi.UnpaddedPieceSize, error) {
	var pcr PieceCommRecord
	err := cm.DB.First(&pcr, "data = ?", data.Bytes()).Error
	if err == nil {
		fmt.Println("database response!!!")
		if !pcr.Piece.CID.Defined() {
			return cid.Undef, 0, fmt.Errorf("got an undefined thing back from database")
		}
		return pcr.Piece.CID, pcr.Size, nil
	}

	if !xerrors.Is(err, gorm.ErrRecordNotFound) {
		return cid.Undef, 0, err
	}

	pc, size, err := filclient.GeneratePieceCommitment(rt, data, bs)
	if err != nil {
		return cid.Undef, 0, xerrors.Errorf("failed to generate piece commitment: %w", err)
	}

	pcr = PieceCommRecord{
		Data:  dbCID{data},
		Piece: dbCID{pc},
		Size:  size,
	}

	if err := cm.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&pcr).Error; err != nil {
		return cid.Undef, 0, err
	}

	return pc, size, nil
}

func (cm *ContentManager) ClearUnused() error {
	// first, gather candidates for removal
	// that is any content we have made the correct number of deals for, that
	// hasnt been fetched from us in X days

	return nil
}

func (cm *ContentManager) getRemovalCandidates() ([]Content, error) {
	var conts []Content
	if err := cm.DB.Find(&conts, "active and not offloaded").Error; err != nil {
		return nil, err
	}

	var toOffload []Content
	for _, c := range conts {
		ok, err := cm.contentIsProperlyReplicated(c.ID)
		if err != nil {
			return nil, xerrors.Errorf("failed to check replication of %d: %w", c.ID, err)
		}

		if !ok {
			// maybe kick off repairs?
			continue
		}

		toOffload = append(toOffload, c)
	}

	return nil, nil
}

func (cm *ContentManager) contentIsProperlyReplicated(c uint) (bool, error) {
	return false, nil
}

/*
"miner": {
        "minerAddr": "f022352",
        "metadata": {
          "location": "NO"
        },
        "filecoin": {
          "relativePower": 0.00005677536865959914,
          "askPrice": "20000000000",
          "askVerifiedPrice": "0",
          "minPieceSize": "17179869184",
          "maxPieceSize": "34359738368",
          "sectorSize": "34359738368",
          "activeSectors": "7487",
          "faultySectors": "1",
          "updatedAt": "2021-04-06T00:03:38.049Z"
        },
        "textile": {
          "regions": {
            "021": {
              "deals": {
                "total": "279",
                "last": "2021-03-24T17:52:01.258Z",
                "failures": "13",
                "lastFailure": "2021-04-04T18:04:43.824Z",
                "tailTransfers": [
                  {
                    "transferedAt": "2021-03-23T21:09:57Z",
                    "mibPerSec": 6.0512123834078535
                  },
                  {
                    "transferedAt": "2021-03-03T18:18:15Z",
                    "mibPerSec": 6.590866735426046
                  }
                ],
                "tailSealed": [
                  {
                    "sealedAt": "2021-03-24T17:52:01Z",
                    "durationSeconds": "6091"
                  },
                  {
                    "sealedAt": "2021-03-04T01:14:13Z",
                    "durationSeconds": "6300"
                  }
                ]
              },
              "retrievals": {
                "total": "1",
                "last": "2021-03-19T20:34:08.289Z",
                "failures": "0",
                "lastFailure": null,
                "tailTransfers": [
                  {
                    "transferedAt": "2021-03-19T20:34:08Z",
                    "mibPerSec": 1.3760741723550332
                  }
                ]
              }
            }
          },
          "dealsSummary": {
            "total": "279",
            "last": "2021-03-24T17:52:01.258Z",
            "failures": "13",
            "lastFailure": "2021-04-04T18:04:43.824Z"
          },
          "retrievalsSummary": {
            "total": "1",
            "last": "2021-03-19T20:34:08.289Z",
            "failures": "0",
            "lastFailure": null
          },
          "updatedAt": "2021-04-06T00:03:32.340Z"
        },
        "updatedAt": "2021-04-06T00:03:32.340Z"
      }
    },
*/
func (cm *ContentManager) pullTextileMinersList() error {
	url := "https://minerindex.hub.textile.io/v1/index/query?sort.ascending=false&sort.field=TEXTILE_DEALS_TOTAL_SUCCESSFUL&limit=100"

	resp, err := http.Get(url)
	if err != nil {
		return err
	}

	if resp.StatusCode != 200 {
		return fmt.Errorf("bad response from textile: %d %s", resp.StatusCode, resp.Status)
	}

	return nil
}

func (cm *ContentManager) FreeStorage() {
}
