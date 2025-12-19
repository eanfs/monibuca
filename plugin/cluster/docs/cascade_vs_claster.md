# Comparison: Cascade vs. Cluster

This document outlines the key differences between the existing Cascade plugin and the new Cluster plugin, highlighting how Cluster extends and enhances Cascade's functionality.

## Overview Comparison

| Feature | Cascade | Cluster |
|---------|---------|---------|
| **Primary Purpose** | Basic server cascading | Full cluster management |
| **Architecture** | Simple client-server | Distributed with multiple roles |
| **Load Balancing** | Basic or none | Advanced, adaptive algorithms |
| **Stream Synchronization** | Limited | Comprehensive with conflict resolution |
| **Failover** | Minimal | Robust with automatic recovery |
| **Resource Optimization** | None | Advanced with predictive capabilities |
| **Monitoring** | Basic | Comprehensive with dashboard |
| **Scaling** | Manual | Semi-automatic with scaling triggers |
| **Configuration** | Simple | Extensive with multiple options |
| **Protocol** | Basic QUIC | Extended protocols (CCP, SSP, LBEP) |

## Detailed Comparison

### 1. Architecture

#### Cascade:
- Simple client-server architecture
- Upper-level and lower-level servers
- Limited to a simple tree structure
- No role differentiation between nodes

#### Cluster:
- Distributed system with multiple node roles
- Manager, edge, worker, and transcoder roles
- Support for complex mesh topologies
- Dynamic role assignment based on capabilities

### 2. Communication Protocol

#### Cascade:
- Basic QUIC-based communication
- Simple message formats
- Limited protocol capabilities
- No specialized protocols for different tasks

#### Cluster:
- Extended QUIC-based communication
- Multiple specialized protocols:
  - Cluster Control Protocol (CCP)
  - Stream Sync Protocol (SSP)
  - Load Balancing Exchange Protocol (LBEP)
- Advanced message formats with vector clocks
- Bloom filters for efficient synchronization

### 3. Load Balancing

#### Cascade:
- No built-in load balancing
- Manual configuration for stream distribution
- No awareness of server load or capacity
- No ability to adapt to changing conditions

#### Cluster:
- Advanced load balancing algorithms
- Automatic publisher routing based on:
  - Current server load
  - Network topology
  - Geographic proximity
  - Stream characteristics
- Dynamic adaptation to changing conditions
- Support for multiple balancing strategies

### 4. Stream Synchronization

#### Cascade:
- Basic stream information passing
- No mechanism for handling conflicts
- Limited metadata synchronization
- No support for stream migration

#### Cluster:
- Comprehensive stream registry
- Vector clock-based conflict resolution
- Efficient synchronization using bloom filters
- Support for seamless stream migration
- Real-time stream state updates across the cluster

### 5. Failover Handling

#### Cascade:
- Limited failover capabilities
- Manual recovery from failures
- No automatic redistribution of streams
- Potential for data loss during failures

#### Cluster:
- Comprehensive failure detection
- Automatic failover procedures
- Stream redistribution after node failures
- Session persistence during failovers
- Configurable recovery strategies

### 6. Resource Optimization

#### Cascade:
- No resource optimization features
- Static resource allocation
- No awareness of stream priorities
- No consideration of hardware capabilities

#### Cluster:
- Intelligent resource allocation
- Stream consolidation for efficiency
- Priority-based resource assignment
- Transcoding distribution across nodes
- Consideration of hardware capabilities
- Predictive scaling based on usage patterns

### 7. Monitoring and Management

#### Cascade:
- Basic status reporting
- Limited metrics collection
- No historical data tracking
- No visualization tools

#### Cluster:
- Comprehensive metrics collection
- Historical data with trend analysis
- Alerting system for critical issues
- Web-based dashboard for visualization
- Detailed per-stream and per-node metrics

### 8. Configuration Options

#### Cascade:
```yaml
cascade:
  server: "192.168.1.100:7001"
  secret: "shared-secret"
  autopush: true
```

#### Cluster:
```yaml
cluster:
  enabled: true
  role: "manager"
  manager_address: "192.168.1.100:7001"
  manager_fallback: "192.168.1.101:7001"
  cluster_secret: "shared-secret-key"
  region: "us-east"
  
  resources:
    max_streams: 1000
    max_bandwidth_mbps: 10000
    reserve_cpu_percent: 20
    reserve_memory_percent: 15
    transcoding_priority: 0.8
  
  load_balancing:
    strategy: "resource-aware"
    check_interval_ms: 1000
    rebalance_threshold: 0.2
    weights:
      cpu: 0.3
      memory: 0.2
      network: 0.3
      stream_count: 0.2
  
  # Many more configuration options...
```

### 9. API Capabilities

#### Cascade:
- Limited API endpoints
- Basic stream and node management
- No cluster-wide operations

#### Cluster:
- Comprehensive REST API
- Full gRPC service definition
- Support for cluster-wide operations
- Fine-grained control over node behavior
- Stream and resource management APIs

### 10. Code Structure

#### Cascade:
```
plugin/cascade/
├── client.go    # Simple client implementation
├── server.go    # Simple server implementation
├── pb/          # Basic protobuf definitions
└── pkg/         # Limited utility functions
```

#### Cluster:
```
plugin/cluster/
├── manager/     # Cluster manager implementation
├── node/        # Node agent implementation
├── protocols/   # Protocol implementations
│   ├── ccp/     # Cluster Control Protocol
│   ├── ssp/     # Stream Sync Protocol
│   └── lbep/    # Load Balancing Exchange Protocol
├── balancer/    # Load balancing algorithms
├── sync/        # Stream synchronization
├── monitoring/  # Monitoring and metrics
├── api/         # API implementations
├── dashboard/   # Web dashboard
├── pb/          # Extensive protobuf definitions
└── pkg/         # Utility functions and shared code
```

## Migration Path

### From Cascade to Cluster

1. **Configuration Migration**:
   - Existing cascade configuration can be automatically converted to cluster format
   - Default values provided for new options

2. **Gradual Deployment**:
   - Cluster can operate in "cascade-compatible" mode
   - Nodes can be upgraded one by one

3. **Data Migration**:
   - Stream information automatically migrated
   - No downtime required for migration

4. **API Compatibility**:
   - Cascade API endpoints remain available
   - New endpoints added for Cluster-specific features

## When to Use Which Plugin

### Use Cascade When:
- Simple server cascading is sufficient
- Limited number of servers (2-3)
- Simple, static configuration is preferred
- Minimal resource requirements are important
- Lower complexity is desired

### Use Cluster When:
- Complex clustering with multiple nodes is needed
- Dynamic load balancing is required
- High availability is important
- Resource optimization is a priority
- Advanced monitoring and management is needed
- Multiple geographic regions are involved
- Automatic failover capabilities are required 