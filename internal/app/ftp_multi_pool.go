package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jlaffaye/ftp"
)

// PooledFTPConnection represents a single FTP connection in the pool
type PooledFTPConnection struct {
	id         int
	conn       *ftp.ServerConn
	inUse      atomic.Bool
	lastUsed   time.Time
	createdAt  time.Time
	useTLS     bool // Track if this connection uses TLS
}

// MultiFTPPool manages multiple FTP connections for parallel operations
type MultiFTPPool struct {
	mu          sync.Mutex
	connections []*PooledFTPConnection
	poolSize    int
	maxIdleTime time.Duration

	// Stats
	totalRequests atomic.Int64
	cacheHits     atomic.Int64

	// Context for graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc
}

var (
	globalMultiFTPPool *MultiFTPPool
	multiFTPPoolOnce   sync.Once
)

// InitMultiFTPPool initializes the multi-connection FTP pool
func InitMultiFTPPool(poolSize int) {
	multiFTPPoolOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())

		if poolSize <= 0 {
			poolSize = getEnvInt("FTP_POOL_SIZE", 4) // Default 4 connections for FTP
		}

		globalMultiFTPPool = &MultiFTPPool{
			connections: make([]*PooledFTPConnection, 0, poolSize),
			poolSize:    poolSize,
			maxIdleTime: 5 * time.Minute,
			ctx:         ctx,
			cancel:      cancel,
		}

		// Pre-create connections in parallel
		log.Printf("🔄 Initializing FTP Multi-Pool with %d connections (parallel)...", poolSize)

		var wg sync.WaitGroup
		connChan := make(chan *PooledFTPConnection, poolSize)

		for i := 0; i < poolSize; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				conn, err := globalMultiFTPPool.createConnection(id)
				if err != nil {
					log.Printf("⚠️ Failed to create FTP connection %d: %v", id, err)
					return
				}
				connChan <- conn
			}(i)
		}

		// Wait for all connections to be created
		go func() {
			wg.Wait()
			close(connChan)
		}()

		// Collect connections
		for conn := range connChan {
			globalMultiFTPPool.connections = append(globalMultiFTPPool.connections, conn)
		}

		// Start monitor and keepalive
		go globalMultiFTPPool.monitor()
		go globalMultiFTPPool.keepAliveLoop()

		log.Printf("✅ FTP Multi-Pool initialized with %d/%d connections",
			len(globalMultiFTPPool.connections), poolSize)
	})
}

// createConnection creates a new pooled FTP connection
func (p *MultiFTPPool) createConnection(id int) (*PooledFTPConnection, error) {
	address := fmt.Sprintf("%s:%s", Config.FTPHost, Config.FTPPort)

	// Check if user wants to force TLS mode
	forceTLS := os.Getenv("FTP_FORCE_TLS") == "true"
	forcePlain := os.Getenv("FTP_FORCE_PLAIN") == "true"

	var conn *ftp.ServerConn
	var err error
	var useTLS bool

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         Config.FTPHost,
		MinVersion:         tls.VersionTLS12,
	}

	if forceTLS {
		// Force Explicit FTPS mode
		conn, err = ftp.Dial(address,
			ftp.DialWithTimeout(30*time.Second),
			ftp.DialWithExplicitTLS(tlsConfig),
			ftp.DialWithDisabledEPSV(true),
		)
		if err != nil {
			return nil, fmt.Errorf("FTPS connection failed: %v", err)
		}
		useTLS = true
	} else if forcePlain {
		// Force plain FTP mode
		conn, err = ftp.Dial(address,
			ftp.DialWithTimeout(30*time.Second),
			ftp.DialWithDisabledEPSV(true),
		)
		if err != nil {
			return nil, fmt.Errorf("plain FTP connection failed: %v", err)
		}
		useTLS = false
	} else {
		// Auto mode: Try plain FTP first, then Explicit FTPS
		conn, err = ftp.Dial(address,
			ftp.DialWithTimeout(30*time.Second),
			ftp.DialWithDisabledEPSV(true),
		)

		if err != nil {
			// Try with Explicit FTPS as fallback
			conn, err = ftp.Dial(address,
				ftp.DialWithTimeout(30*time.Second),
				ftp.DialWithExplicitTLS(tlsConfig),
				ftp.DialWithDisabledEPSV(true),
			)
			if err != nil {
				return nil, fmt.Errorf("FTP connection failed (both plain and TLS): %v", err)
			}
			useTLS = true
		} else {
			useTLS = false
		}
	}

	// Login
	err = conn.Login(Config.FTPUser, Config.FTPPassword)
	if err != nil {
		conn.Quit()
		return nil, fmt.Errorf("FTP login failed: %v", err)
	}

	// Set binary mode
	if err := conn.Type(ftp.TransferTypeBinary); err != nil {
		log.Printf("⚠️ Failed to set binary mode for connection #%d: %v", id, err)
	}

	pooledConn := &PooledFTPConnection{
		id:        id,
		conn:      conn,
		lastUsed:  time.Now(),
		createdAt: time.Now(),
		useTLS:    useTLS,
	}

	tlsStatus := "plain"
	if useTLS {
		tlsStatus = "TLS"
	}
	log.Printf("✅ FTP Pool connection #%d established (%s)", id, tlsStatus)

	return pooledConn, nil
}

// keepAliveLoop sends NOOP commands periodically to all idle connections
func (p *MultiFTPPool) keepAliveLoop() {
	ticker := time.NewTicker(90 * time.Second) // Send NOOP every 90 seconds
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.mu.Lock()
			for _, conn := range p.connections {
				// Only send keepalive if connection is not in use
				if conn.conn != nil && !conn.inUse.Load() {
					if err := conn.conn.NoOp(); err != nil {
						log.Printf("⚠️ FTP keepalive failed for connection #%d: %v", conn.id, err)
						// Mark connection as dead - will be recreated on next use
						conn.conn = nil
					}
				}
			}
			p.mu.Unlock()
		}
	}
}

// AcquireConnection gets an available connection from the pool
func (p *MultiFTPPool) AcquireConnection() (*ftp.ServerConn, int, error) {
	p.totalRequests.Add(1)

	p.mu.Lock()
	defer p.mu.Unlock()

	// Try to find an available connection
	for _, conn := range p.connections {
		if conn.inUse.CompareAndSwap(false, true) {
			// Test if connection is still alive
			if conn.conn != nil {
				if err := conn.conn.NoOp(); err == nil {
					conn.lastUsed = time.Now()
					p.cacheHits.Add(1)
					return conn.conn, conn.id, nil
				}
			}

			// Connection is dead, recreate it
			log.Printf("⚠️ FTP Connection #%d is dead, recreating...", conn.id)
			p.recreateConnection(conn)

			if conn.conn != nil {
				conn.lastUsed = time.Now()
				return conn.conn, conn.id, nil
			}

			conn.inUse.Store(false)
		}
	}

	// All connections are busy, try to create a new one if under limit
	if len(p.connections) < p.poolSize {
		newConn, err := p.createConnection(len(p.connections))
		if err != nil {
			return nil, -1, fmt.Errorf("failed to create new FTP connection: %v", err)
		}
		newConn.inUse.Store(true)
		p.connections = append(p.connections, newConn)
		return newConn.conn, newConn.id, nil
	}

	// All connections busy and at max pool size - wait and retry
	p.mu.Unlock()

	// Wait and retry (30 x 200ms = 6 seconds max wait)
	for i := 0; i < 30; i++ {
		time.Sleep(200 * time.Millisecond)

		p.mu.Lock()
		for _, conn := range p.connections {
			if conn.inUse.CompareAndSwap(false, true) {
				if conn.conn != nil {
					if err := conn.conn.NoOp(); err == nil {
						conn.lastUsed = time.Now()
						p.mu.Unlock()
						return conn.conn, conn.id, nil
					}
				}
				conn.inUse.Store(false)
			}
		}
		p.mu.Unlock()
	}

	p.mu.Lock() // Re-lock for defer
	return nil, -1, fmt.Errorf("no available FTP connections after waiting")
}

// ReleaseConnection marks a connection as available
func (p *MultiFTPPool) ReleaseConnection(connID int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, conn := range p.connections {
		if conn.id == connID {
			conn.lastUsed = time.Now()
			conn.inUse.Store(false)
			return
		}
	}
}

// recreateConnection closes and recreates a dead connection
func (p *MultiFTPPool) recreateConnection(pooledConn *PooledFTPConnection) {
	// Close old connection
	if pooledConn.conn != nil {
		pooledConn.conn.Quit()
		pooledConn.conn = nil
	}

	// Create new connection
	address := fmt.Sprintf("%s:%s", Config.FTPHost, Config.FTPPort)

	forceTLS := os.Getenv("FTP_FORCE_TLS") == "true"
	forcePlain := os.Getenv("FTP_FORCE_PLAIN") == "true"

	var conn *ftp.ServerConn
	var err error
	var useTLS bool

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         Config.FTPHost,
		MinVersion:         tls.VersionTLS12,
	}

	if forceTLS {
		conn, err = ftp.Dial(address,
			ftp.DialWithTimeout(30*time.Second),
			ftp.DialWithExplicitTLS(tlsConfig),
			ftp.DialWithDisabledEPSV(true),
		)
		useTLS = true
	} else if forcePlain {
		conn, err = ftp.Dial(address,
			ftp.DialWithTimeout(30*time.Second),
			ftp.DialWithDisabledEPSV(true),
		)
		useTLS = false
	} else {
		// Auto mode - try plain first
		conn, err = ftp.Dial(address,
			ftp.DialWithTimeout(30*time.Second),
			ftp.DialWithDisabledEPSV(true),
		)
		if err != nil {
			conn, err = ftp.Dial(address,
				ftp.DialWithTimeout(30*time.Second),
				ftp.DialWithExplicitTLS(tlsConfig),
				ftp.DialWithDisabledEPSV(true),
			)
			useTLS = true
		}
	}

	if err != nil {
		log.Printf("❌ Failed to recreate FTP connection #%d: %v", pooledConn.id, err)
		return
	}

	// Login
	if err := conn.Login(Config.FTPUser, Config.FTPPassword); err != nil {
		conn.Quit()
		log.Printf("❌ Failed to login for FTP connection #%d: %v", pooledConn.id, err)
		return
	}

	// Set binary mode
	conn.Type(ftp.TransferTypeBinary)

	pooledConn.conn = conn
	pooledConn.createdAt = time.Now()
	pooledConn.useTLS = useTLS

	tlsStatus := "plain"
	if useTLS {
		tlsStatus = "TLS"
	}
	log.Printf("✅ FTP Connection #%d recreated successfully (%s)", pooledConn.id, tlsStatus)
}

// monitor checks connections and prints stats
func (p *MultiFTPPool) monitor() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.mu.Lock()

			activeCount := 0
			idleCount := 0

			for _, conn := range p.connections {
				if conn.inUse.Load() {
					activeCount++
				} else {
					idleCount++
				}
			}

			totalReq := p.totalRequests.Load()
			hits := p.cacheHits.Load()
			hitRate := float64(0)
			if totalReq > 0 {
				hitRate = float64(hits) / float64(totalReq) * 100
			}

			log.Printf("📊 FTP Pool Stats: %d active, %d idle, %d total | Requests: %d, Hit rate: %.1f%%",
				activeCount, idleCount, len(p.connections), totalReq, hitRate)

			p.mu.Unlock()
		}
	}
}

// CloseMultiFTPPool closes all FTP connections
func CloseMultiFTPPool() {
	if globalMultiFTPPool != nil {
		globalMultiFTPPool.cancel()

		globalMultiFTPPool.mu.Lock()
		for _, conn := range globalMultiFTPPool.connections {
			if conn.conn != nil {
				conn.conn.Quit()
			}
		}
		globalMultiFTPPool.connections = nil
		globalMultiFTPPool.mu.Unlock()

		log.Printf("🔌 FTP Multi-Pool closed")
	}
}

// GetMultiFTPConnection is the main function to get a pooled FTP connection
// Returns connection and a release function
// IMPORTANT: Caller MUST call the release function when done!
func GetMultiFTPConnection() (*ftp.ServerConn, func(), error) {
	if globalMultiFTPPool == nil {
		InitMultiFTPPool(0) // Use default pool size
	}

	client, connID, err := globalMultiFTPPool.AcquireConnection()
	if err != nil {
		return nil, nil, err
	}

	// Return release function
	releaseFunc := func() {
		globalMultiFTPPool.ReleaseConnection(connID)
	}

	return client, releaseFunc, nil
}

// GetFTPPoolStats returns current FTP pool statistics
func GetFTPPoolStats() (total, active, idle int, hitRate float64) {
	if globalMultiFTPPool == nil {
		return 0, 0, 0, 0
	}

	globalMultiFTPPool.mu.Lock()
	defer globalMultiFTPPool.mu.Unlock()

	total = len(globalMultiFTPPool.connections)
	for _, conn := range globalMultiFTPPool.connections {
		if conn.inUse.Load() {
			active++
		} else {
			idle++
		}
	}

	totalReq := globalMultiFTPPool.totalRequests.Load()
	hits := globalMultiFTPPool.cacheHits.Load()
	if totalReq > 0 {
		hitRate = float64(hits) / float64(totalReq) * 100
	}

	return
}
