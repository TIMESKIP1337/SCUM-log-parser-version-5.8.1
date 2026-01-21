package app

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// PooledConnection represents a single connection in the pool
type PooledConnection struct {
	id          int
	sftpClient  *sftp.Client
	sshClient   *ssh.Client
	inUse       atomic.Bool
	lastUsed    time.Time
	createdAt   time.Time
	keepAliveCtx    context.Context
	keepAliveCancel context.CancelFunc
}

// MultiSFTPPool manages multiple SFTP connections for parallel operations
type MultiSFTPPool struct {
	mu          sync.Mutex
	connections []*PooledConnection
	poolSize    int
	maxIdleTime time.Duration

	// Stats
	totalRequests   atomic.Int64
	cacheHits       atomic.Int64

	// Context for graceful shutdown
	ctx    context.Context
	cancel context.CancelFunc
}

var (
	globalMultiPool *MultiSFTPPool
	multiPoolOnce   sync.Once
)

// InitMultiSFTPPool initializes the multi-connection SFTP pool
func InitMultiSFTPPool(poolSize int) {
	multiPoolOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())

		if poolSize <= 0 {
			poolSize = getEnvInt("SFTP_POOL_SIZE", 6) // Default 6 connections
		}

		globalMultiPool = &MultiSFTPPool{
			connections: make([]*PooledConnection, 0, poolSize),
			poolSize:    poolSize,
			maxIdleTime: 5 * time.Minute,
			ctx:         ctx,
			cancel:      cancel,
		}

		// Pre-create connections in parallel (much faster startup)
		log.Printf("🔄 Initializing SFTP Multi-Pool with %d connections (parallel)...", poolSize)

		var wg sync.WaitGroup
		connChan := make(chan *PooledConnection, poolSize)

		for i := 0; i < poolSize; i++ {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				conn, err := globalMultiPool.createConnection(id)
				if err != nil {
					log.Printf("⚠️ Failed to create connection %d: %v", id, err)
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
			globalMultiPool.connections = append(globalMultiPool.connections, conn)
		}

		// Start monitor
		go globalMultiPool.monitor()

		log.Printf("✅ SFTP Multi-Pool initialized with %d/%d connections",
			len(globalMultiPool.connections), poolSize)
	})
}

// createConnection creates a new pooled connection
func (p *MultiSFTPPool) createConnection(id int) (*PooledConnection, error) {
	sshConfig := &ssh.ClientConfig{
		User: Config.FTPUser,
		Auth: []ssh.AuthMethod{
			ssh.Password(Config.FTPPassword),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}

	sshClient, err := ssh.Dial("tcp", fmt.Sprintf("%s:%s", Config.FTPHost, Config.FTPPort), sshConfig)
	if err != nil {
		return nil, fmt.Errorf("SSH connection failed: %v", err)
	}

	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		sshClient.Close()
		return nil, fmt.Errorf("SFTP client creation failed: %v", err)
	}

	ctx, cancel := context.WithCancel(p.ctx)

	conn := &PooledConnection{
		id:              id,
		sftpClient:      sftpClient,
		sshClient:       sshClient,
		lastUsed:        time.Now(),
		createdAt:       time.Now(),
		keepAliveCtx:    ctx,
		keepAliveCancel: cancel,
	}

	// Start keep-alive for this connection
	go p.runKeepAlive(conn)

	log.Printf("✅ Pool connection #%d established", id)
	return conn, nil
}

// runKeepAlive sends keep-alive packets for a specific connection
func (p *MultiSFTPPool) runKeepAlive(conn *PooledConnection) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-conn.keepAliveCtx.Done():
			return
		case <-ticker.C:
			if conn.sshClient != nil && !conn.inUse.Load() {
				_, _, err := conn.sshClient.SendRequest("keepalive@openssh.com", true, nil)
				if err != nil {
					log.Printf("⚠️ Keep-alive failed for connection #%d: %v", conn.id, err)
					return
				}
			}
		}
	}
}

// AcquireConnection gets an available connection from the pool
func (p *MultiSFTPPool) AcquireConnection() (*sftp.Client, int, error) {
	p.totalRequests.Add(1)

	p.mu.Lock()
	defer p.mu.Unlock()

	// Try to find an available connection
	for _, conn := range p.connections {
		if conn.inUse.CompareAndSwap(false, true) {
			// Test if connection is still alive
			if _, err := conn.sftpClient.Stat("."); err == nil {
				conn.lastUsed = time.Now()
				p.cacheHits.Add(1)
				return conn.sftpClient, conn.id, nil
			}

			// Connection is dead, recreate it
			log.Printf("⚠️ Connection #%d is dead, recreating...", conn.id)
			p.recreateConnection(conn)

			if conn.sftpClient != nil {
				conn.lastUsed = time.Now()
				return conn.sftpClient, conn.id, nil
			}

			conn.inUse.Store(false)
		}
	}

	// All connections are busy, try to create a new one if under limit
	if len(p.connections) < p.poolSize {
		newConn, err := p.createConnection(len(p.connections))
		if err != nil {
			return nil, -1, fmt.Errorf("failed to create new connection: %v", err)
		}
		newConn.inUse.Store(true)
		p.connections = append(p.connections, newConn)
		return newConn.sftpClient, newConn.id, nil
	}

	// All connections busy and at max pool size - wait and retry
	p.mu.Unlock()

	// Wait longer and retry (30 x 200ms = 6 seconds max wait)
	for i := 0; i < 30; i++ {
		time.Sleep(200 * time.Millisecond)

		p.mu.Lock()
		for _, conn := range p.connections {
			if conn.inUse.CompareAndSwap(false, true) {
				if _, err := conn.sftpClient.Stat("."); err == nil {
					conn.lastUsed = time.Now()
					p.mu.Unlock()
					return conn.sftpClient, conn.id, nil
				}
				conn.inUse.Store(false)
			}
		}
		p.mu.Unlock()
	}

	p.mu.Lock() // Re-lock for defer
	return nil, -1, fmt.Errorf("no available connections after waiting")
}

// ReleaseConnection marks a connection as available
func (p *MultiSFTPPool) ReleaseConnection(connID int) {
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
func (p *MultiSFTPPool) recreateConnection(conn *PooledConnection) {
	// Close old connection
	if conn.keepAliveCancel != nil {
		conn.keepAliveCancel()
	}
	if conn.sftpClient != nil {
		conn.sftpClient.Close()
	}
	if conn.sshClient != nil {
		conn.sshClient.Close()
	}

	// Create new connection
	sshConfig := &ssh.ClientConfig{
		User: Config.FTPUser,
		Auth: []ssh.AuthMethod{
			ssh.Password(Config.FTPPassword),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         30 * time.Second,
	}

	sshClient, err := ssh.Dial("tcp", fmt.Sprintf("%s:%s", Config.FTPHost, Config.FTPPort), sshConfig)
	if err != nil {
		log.Printf("❌ Failed to recreate SSH connection #%d: %v", conn.id, err)
		conn.sftpClient = nil
		conn.sshClient = nil
		return
	}

	sftpClient, err := sftp.NewClient(sshClient)
	if err != nil {
		sshClient.Close()
		log.Printf("❌ Failed to recreate SFTP connection #%d: %v", conn.id, err)
		conn.sftpClient = nil
		conn.sshClient = nil
		return
	}

	ctx, cancel := context.WithCancel(p.ctx)

	conn.sshClient = sshClient
	conn.sftpClient = sftpClient
	conn.createdAt = time.Now()
	conn.keepAliveCtx = ctx
	conn.keepAliveCancel = cancel

	go p.runKeepAlive(conn)

	log.Printf("✅ Connection #%d recreated successfully", conn.id)
}

// monitor checks connections and prints stats
func (p *MultiSFTPPool) monitor() {
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

					// Close very idle connections (keep at least 2)
					if len(p.connections) > 2 && time.Since(conn.lastUsed) > p.maxIdleTime*2 {
						log.Printf("🔌 Closing very idle connection #%d", conn.id)
						if conn.keepAliveCancel != nil {
							conn.keepAliveCancel()
						}
						if conn.sftpClient != nil {
							conn.sftpClient.Close()
						}
						if conn.sshClient != nil {
							conn.sshClient.Close()
						}
					}
				}
			}

			totalReq := p.totalRequests.Load()
			hits := p.cacheHits.Load()
			hitRate := float64(0)
			if totalReq > 0 {
				hitRate = float64(hits) / float64(totalReq) * 100
			}

			log.Printf("📊 SFTP Pool Stats: %d active, %d idle, %d total | Requests: %d, Hit rate: %.1f%%",
				activeCount, idleCount, len(p.connections), totalReq, hitRate)

			p.mu.Unlock()
		}
	}
}

// CloseMultiSFTPPool closes all connections
func CloseMultiSFTPPool() {
	if globalMultiPool != nil {
		globalMultiPool.cancel()

		globalMultiPool.mu.Lock()
		for _, conn := range globalMultiPool.connections {
			if conn.keepAliveCancel != nil {
				conn.keepAliveCancel()
			}
			if conn.sftpClient != nil {
				conn.sftpClient.Close()
			}
			if conn.sshClient != nil {
				conn.sshClient.Close()
			}
		}
		globalMultiPool.connections = nil
		globalMultiPool.mu.Unlock()

		log.Printf("🔌 SFTP Multi-Pool closed")
	}
}

// GetMultiSFTPConnection is a drop-in replacement for GetSFTPConnection
// Returns connection and a release function
func GetMultiSFTPConnection() (*sftp.Client, func(), error) {
	if globalMultiPool == nil {
		InitMultiSFTPPool(0) // Use default pool size
	}

	client, connID, err := globalMultiPool.AcquireConnection()
	if err != nil {
		return nil, nil, err
	}

	// Return release function
	releaseFunc := func() {
		globalMultiPool.ReleaseConnection(connID)
	}

	return client, releaseFunc, nil
}

// GetPoolStats returns current pool statistics
func GetPoolStats() (total, active, idle int, hitRate float64) {
	if globalMultiPool == nil {
		return 0, 0, 0, 0
	}

	globalMultiPool.mu.Lock()
	defer globalMultiPool.mu.Unlock()

	total = len(globalMultiPool.connections)
	for _, conn := range globalMultiPool.connections {
		if conn.inUse.Load() {
			active++
		} else {
			idle++
		}
	}

	totalReq := globalMultiPool.totalRequests.Load()
	hits := globalMultiPool.cacheHits.Load()
	if totalReq > 0 {
		hitRate = float64(hits) / float64(totalReq) * 100
	}

	return
}
