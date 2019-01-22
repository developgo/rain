// Package torrent provides a BitTorrent client implementation for downlaoding a single torrent.
package torrent

import (
	"encoding/hex"
	"net"
	"sync"
	"time"

	"github.com/cenkalti/rain/internal/logger"
	"github.com/cenkalti/rain/internal/torrent/bitfield"
	"github.com/cenkalti/rain/internal/torrent/blocklist"
	"github.com/cenkalti/rain/internal/torrent/dht"
	"github.com/cenkalti/rain/internal/torrent/internal/acceptor"
	"github.com/cenkalti/rain/internal/torrent/internal/addrlist"
	"github.com/cenkalti/rain/internal/torrent/internal/allocator"
	"github.com/cenkalti/rain/internal/torrent/internal/announcer"
	"github.com/cenkalti/rain/internal/torrent/internal/handshaker/incominghandshaker"
	"github.com/cenkalti/rain/internal/torrent/internal/handshaker/outgoinghandshaker"
	"github.com/cenkalti/rain/internal/torrent/internal/infodownloader"
	"github.com/cenkalti/rain/internal/torrent/internal/peer"
	"github.com/cenkalti/rain/internal/torrent/internal/piece"
	"github.com/cenkalti/rain/internal/torrent/internal/piececache"
	"github.com/cenkalti/rain/internal/torrent/internal/piecedownloader"
	"github.com/cenkalti/rain/internal/torrent/internal/piecepicker"
	"github.com/cenkalti/rain/internal/torrent/internal/piecewriter"
	"github.com/cenkalti/rain/internal/torrent/internal/verifier"
	"github.com/cenkalti/rain/internal/torrent/metainfo"
	"github.com/cenkalti/rain/internal/torrent/resumer"
	"github.com/cenkalti/rain/internal/torrent/storage"
	"github.com/cenkalti/rain/internal/tracker"
)

var (
	// We send this in handshake tell supported extensions.
	ourExtensions = bitfield.New(64)
)

func init() {
	ourExtensions.Set(61) // Fast Extension (BEP 6)
	ourExtensions.Set(43) // Extension Protocol (BEP 10)
}

// Torrent connects to peers and downloads files from swarm.
type Torrent struct {
	config Config

	// Identifies the torrent being downloaded.
	infoHash [20]byte

	// List of addresses to announce this torrent.
	trackers []tracker.Tracker

	// Name of the torrent.
	name string

	// Storage implementation to save the files in torrent.
	storage storage.Storage

	// TCP Port to listen for peer connections.
	port int

	// Optional DB implementation to save resume state of the torrent.
	resume resumer.Resumer

	// Contains info about files in torrent. This can be nil at start for magnet downloads.
	info *metainfo.Info

	// Bitfield for pieces we have. It is created after we got info.
	bitfield *bitfield.Bitfield

	// Unique peer ID is generated per downloader.
	peerID [20]byte

	files  []storage.File
	pieces []piece.Piece

	piecePicker *piecepicker.PiecePicker

	// Peers are sent to this channel when they are disconnected.
	peerDisconnectedC chan *peer.Peer

	// Piece messages coming from peers are sent this channel.
	pieceMessages chan peer.PieceMessage

	// To limit parallel writes to disk, pieceMessages is set to nil when a piece has started writing to disk.
	blockPieceMessages chan peer.PieceMessage

	// Other messages coming from peers are sent to this channel.
	messages chan peer.Message

	// We keep connected peers in this map after they complete handshake phase.
	peers map[*peer.Peer]struct{}

	// Also keep a reference to incoming and outgoing peers seperately to count them quickly.
	incomingPeers map[*peer.Peer]struct{}
	outgoingPeers map[*peer.Peer]struct{}
	peersSnubbed  map[*peer.Peer]struct{}

	// Active piece downloads are kept in this map.
	pieceDownloaders        map[*peer.Peer]*piecedownloader.PieceDownloader
	pieceDownloadersSnubbed map[*peer.Peer]*piecedownloader.PieceDownloader
	pieceDownloadersChoked  map[*peer.Peer]*piecedownloader.PieceDownloader

	// When a peer has snubbed us, a message sent to this channel.
	peerSnubbedC chan *peer.Peer

	// Active metadata downloads are kept in this map.
	infoDownloaders        map[*peer.Peer]*infodownloader.InfoDownloader
	infoDownloadersSnubbed map[*peer.Peer]*infodownloader.InfoDownloader

	pieceWriterResultC chan *piecewriter.PieceWriter

	// Some peers are optimistically unchoked regardless of their download rate.
	optimisticUnchokedPeers []*peer.Peer

	// This channel is closed once all pieces are downloaded and verified.
	completeC chan struct{}

	// True after all pieces are download, verified and written to disk.
	completed bool

	// If any unrecoverable error occurs, it will be sent to this channel and download will be stopped.
	errC chan error

	// After listener has started, port will be sent to this channel.
	portC chan int

	// Contains the last error sent to errC.
	lastError error

	// When Stop() is called, it will close this channel to signal run() function to stop.
	closeC chan chan struct{}

	// These are the channels for sending a message to run() loop.
	statsCommandC        chan statsRequest        // Stats()
	trackersCommandC     chan trackersRequest     // Trackers()
	peersCommandC        chan peersRequest        // Peers()
	startCommandC        chan struct{}            // Start()
	stopCommandC         chan struct{}            // Stop()
	notifyErrorCommandC  chan notifyErrorCommand  // NotifyError()
	notifyListenCommandC chan notifyListenCommand // NotifyListen()
	addPeersCommandC     chan []*net.TCPAddr      // AddPeers()

	// Trackers send announce responses to this channel.
	addrsFromTrackers chan []*net.TCPAddr

	// Keeps a list of peer addresses to connect.
	addrList *addrlist.AddrList

	// New raw connections created by OutgoingHandshaker are sent to here.
	incomingConnC chan net.Conn

	// Keep a set of peer IDs to block duplicate connections.
	peerIDs map[[20]byte]struct{}

	// Listens for incoming peer connections.
	acceptor *acceptor.Acceptor

	// Special hash of info hash for encypted connection handshake.
	sKeyHash [20]byte

	// Announces the status of torrent to trackers to get peer addresses periodically.
	announcers []*announcer.PeriodicalAnnouncer

	// This announcer announces Stopped event to the trackers after
	// all periodical trackers are closed.
	stoppedEventAnnouncer *announcer.StopAnnouncer

	// If not nil, torrent is announced to DHT periodically.
	dhtNode      dht.DHT
	dhtAnnouncer *announcer.DHTAnnouncer
	dhtPeersC    chan []*net.TCPAddr

	// List of peers in handshake state.
	incomingHandshakers map[*incominghandshaker.IncomingHandshaker]struct{}
	outgoingHandshakers map[*outgoinghandshaker.OutgoingHandshaker]struct{}

	// Handshake results are sent to these channels by handshakers.
	incomingHandshakerResultC chan *incominghandshaker.IncomingHandshaker
	outgoingHandshakerResultC chan *outgoinghandshaker.OutgoingHandshaker

	// When metadata of the torrent downloaded completely, a message is sent to this channel.
	infoDownloaderResultC chan *infodownloader.InfoDownloader

	// Announcers send a request to this channel to get information about the torrent.
	announcerRequestC chan *announcer.Request

	// A timer that ticks periodically to keep a certain number of peers unchoked.
	unchokeTimer  *time.Ticker
	unchokeTimerC <-chan time.Time

	// A timer that ticks periodically to keep a random peer unchoked regardless of its upload rate.
	optimisticUnchokeTimer  *time.Ticker
	optimisticUnchokeTimerC <-chan time.Time

	// A worker that opens and allocates files on the disk.
	allocator          *allocator.Allocator
	allocatorProgressC chan allocator.Progress
	allocatorResultC   chan *allocator.Allocator

	// A worker that does hash check of files on the disk.
	verifier          *verifier.Verifier
	verifierProgressC chan verifier.Progress
	verifierResultC   chan *verifier.Verifier

	byteStats resumer.Stats

	// Holds connected peer IPs so we don't dial/accept multiple connections to/from same IP.
	connectedPeerIPs map[string]struct{}

	// A signal sent to run() loop when announcers are stopped.
	announcersStoppedC chan struct{}

	// Piece buffers that are being downloaded are pooled to reduce load on GC.
	piecePool sync.Pool

	// Keep a timer to write bitfield at interval to reduce IO.
	resumeWriteTimer  *time.Timer
	resumeWriteTimerC <-chan time.Time

	// Stats are written at interval to reduce IO.
	statsWriteTicker  *time.Ticker
	statsWriteTickerC <-chan time.Time

	// Keeps blocks read from disk in memory.
	pieceCache *piececache.Cache

	// To limit parallel disk reads.
	readMutex sync.Mutex

	// Optional list of IP addresses to block.
	blocklist blocklist.Blocklist

	log logger.Logger
}

// Name of the torrent.
// For magnet downloads name can change after metadata is downloaded but this method still returns the initial name.
// Use Stats() method to get name in info dictionary.
func (t *Torrent) Name() string {
	return t.name
}

// InfoHash string encoded in hex as 40 charachters.
// InfoHash is a unique value that identifies the files in torrent.
func (t *Torrent) InfoHash() string {
	return hex.EncodeToString(t.infoHash[:])
}

// InfoHashBytes return info hash as 20 bytes.
func (t *Torrent) InfoHashBytes() []byte {
	b := make([]byte, 20)
	copy(b, t.infoHash[:])
	return b
}