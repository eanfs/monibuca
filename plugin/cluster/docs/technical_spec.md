# Cluster Plugin Technical Specification

## 1. Introduction

This technical specification details the implementation aspects of the Cluster plugin for server clustering. It focuses on the technical requirements, protocols, and algorithms used to implement load balancing, stream synchronization, and resource optimization.

## 2. System Requirements

### 2.1 Hardware Requirements
- Minimum of 2 cores CPU per node
- 4GB RAM minimum per node 
- 100Mbps network connection minimum between nodes
- 1Gbps recommended for high-traffic nodes
- Low-latency connection between cluster nodes (<50ms)

### 2.2 Software Requirements
- Go 1.16 or later
- Compatible with the core media server
- QUIC support
- Access to system metrics (CPU, memory, network)

## 3. Protocol Specifications

### 3.1 Cluster Control Protocol (CCP)

The CCP is used for node management, health checks, and administrative commands.

#### 3.1.1 Message Format

All CCP messages follow this structure:

```go
type CCPMessage struct {
    Version     uint8
    MessageType uint8
    SequenceNum uint32
    NodeID      string
    Timestamp   int64
    Payload     []byte
    Signature   []byte
}
```

#### 3.1.2 Message Types

| Type | Value | Description |
|------|-------|-------------|
| HEARTBEAT | 0x01 | Regular health check message |
| NODE_JOIN | 0x02 | Request to join cluster |
| NODE_LEAVE | 0x03 | Notification of leaving cluster |
| NODE_INFO | 0x04 | Node information update |
| CLUSTER_STATE | 0x05 | Complete cluster state |
| COMMAND | 0x06 | Administrative command |
| COMMAND_RESPONSE | 0x07 | Response to command |
| ERROR | 0xFF | Error message |

#### 3.1.3 Heartbeat Protocol

1. Each node sends a HEARTBEAT message every `heartbeat_interval_ms` milliseconds
2. If no heartbeat is received for `heartbeat_timeout_ms`, node is considered potentially offline
3. After `node_failure_threshold` missed heartbeats, node is considered offline
4. Recovery requires a new NODE_JOIN sequence

#### 3.1.4 Authentication

All CCP messages are authenticated using HMAC-SHA256:

```
signature = HMAC-SHA256(shared_secret, Version + MessageType + SequenceNum + NodeID + Timestamp + Payload)
```

### 3.2 Stream Sync Protocol (SSP)

The SSP is used for synchronizing stream information between nodes.

#### 3.2.1 Message Format

```go
type SSPMessage struct {
    Version     uint8
    MessageType uint8
    NodeID      string
    Timestamp   int64
    StreamCount uint16
    Streams     []StreamInfo
    BloomFilter []byte  // Used for efficient difference detection
    VectorClock map[string]uint64
    Signature   []byte
}
```

#### 3.2.2 Message Types

| Type | Value | Description |
|------|-------|-------------|
| SYNC_REQUEST | 0x01 | Request for stream synchronization |
| SYNC_RESPONSE | 0x02 | Response with stream information |
| DELTA_UPDATE | 0x03 | Only changed stream information |
| STREAM_STATUS | 0x04 | Status change for a specific stream |
| FULL_SYNC | 0x05 | Complete stream registry |
| BLOOM_FILTER | 0x06 | Bloom filter for difference detection |

#### 3.2.3 Stream Registration Process

1. When a node receives a new stream, it creates a StreamInfo record
2. The node broadcasts a DELTA_UPDATE message to all other nodes
3. Receiving nodes update their local registry
4. Periodically, nodes exchange BLOOM_FILTER messages to detect inconsistencies
5. If inconsistencies are detected, a SYNC_REQUEST/RESPONSE sequence is initiated
6. Full synchronization occurs at configurable intervals

### 3.3 Load Balancing Exchange Protocol (LBEP)

The LBEP is used for exchanging load metrics and making balancing decisions.

#### 3.3.1 Message Format

```go
type LBEPMessage struct {
    Version     uint8
    MessageType uint8
    NodeID      string
    Timestamp   int64
    LoadMetrics ResourceUsage
    DecisionID  string
    Decisions   []LoadBalancingDecision
    Signature   []byte
}

type LoadBalancingDecision struct {
    DecisionType    uint8
    StreamPath      string
    SourceNodeID    string
    DestinationNodeID string
    Priority        uint8
    Timestamp       int64
}
```

#### 3.3.2 Message Types

| Type | Value | Description |
|------|-------|-------------|
| LOAD_UPDATE | 0x01 | Node load metrics update |
| DECISION_PROPOSAL | 0x02 | Proposed load balancing action |
| DECISION_CONFIRM | 0x03 | Confirmation of load balancing action |
| DECISION_REJECT | 0x04 | Rejection of load balancing action |
| CAPACITY_UPDATE | 0x05 | Node capacity change notification |

#### 3.3.3 Load Balancing Decision Process

1. Nodes periodically send LOAD_UPDATE messages to the Cluster Manager
2. The Cluster Manager analyzes load distribution across the cluster
3. If imbalance is detected, it creates a DECISION_PROPOSAL
4. Affected nodes respond with DECISION_CONFIRM or DECISION_REJECT
5. Confirmed decisions are executed by the relevant nodes

## 4. Algorithm Specifications

### 4.1 Load Balancing Algorithm

The load balancing algorithm uses a weighted scoring approach to determine the optimal node for stream placement.

#### 4.1.1 Node Score Calculation

For each node, a score is calculated as:

```
score = (w_cpu * cpu_factor) + (w_mem * mem_factor) + (w_net * net_factor) + 
        (w_streams * stream_factor) + (w_topo * topology_factor)
```

Where:
- `w_cpu`, `w_mem`, `w_net`, `w_streams`, `w_topo` are configurable weights
- Each factor is normalized to a 0-1 range

#### 4.1.2 Factor Calculations

```
cpu_factor = 1 - (current_cpu_percent / max_cpu_percent)
mem_factor = 1 - (current_mem_usage / max_mem_usage)
net_factor = 1 - (current_bandwidth / max_bandwidth)
stream_factor = 1 - (current_stream_count / max_stream_count)
```

The topology factor is calculated based on network latency:

```
topology_factor = 1 - (latency_to_client / max_acceptable_latency)
```

#### 4.1.3 Publisher Placement Algorithm

1. When a new publisher connects, calculate scores for all available nodes
2. Select the node with the highest score
3. If the highest score is below a configurable threshold, trigger cluster scaling (if enabled)
4. Update load metrics after placement

#### 4.1.4 Subscriber Redirection Algorithm

1. When a subscriber requests a stream, locate all nodes with that stream
2. Calculate scores for each node, with additional weight for nodes already serving the stream
3. Direct the subscriber to the node with the highest score
4. If necessary, replicate the stream to the selected node before connecting the subscriber

### 4.2 Stream Synchronization Algorithm

The stream synchronization uses a modified gossip protocol with bloom filters for efficiency.

#### 4.2.1 Bloom Filter Configuration

Each node maintains a bloom filter of its stream registry:

```
m = -n*ln(p)/(ln(2))²
k = m/n*ln(2)
```

Where:
- `m` is the size of the bit array
- `n` is the expected number of streams
- `p` is the desired false positive probability
- `k` is the number of hash functions

#### 4.2.2 Delta Synchronization Process

1. When a stream is added, modified, or removed, the node updates its local registry
2. The node broadcasts a DELTA_UPDATE message to its peers
3. Peers update their local registries and bloom filters

#### 4.2.3 Inconsistency Detection

1. Periodically, nodes exchange bloom filters
2. Each node tests its stream registry entries against received bloom filters
3. If an entry is not found in a peer's bloom filter, it is added to a list of potentially missing streams
4. For each potentially missing stream, a direct query is sent to confirm the inconsistency
5. Confirmed missing streams are sent as DELTA_UPDATE messages

#### 4.2.4 Conflict Resolution

When conflicting stream information is detected:

1. Compare vector clock entries for the stream
2. The entry with the latest vector clock is considered authoritative
3. If vector clocks are identical, use the node with the lower ID as a tiebreaker
4. Apply the winning entry and update local vector clock

### 4.3 Resource Optimization Algorithm

The resource optimization algorithm aims to maximize resource utilization while ensuring stream quality.

#### 4.3.1 Stream Consolidation

1. Periodically analyze stream distribution across nodes
2. Identify nodes with low resource utilization
3. For each low-utilization node, calculate the cost of moving its streams to other nodes
4. If the total cost is below a threshold, propose stream migration
5. After migration, the node can be temporarily deactivated to save resources

#### 4.3.2 Stream Priority Calculation

Each stream is assigned a priority score:

```
priority = (viewer_count * viewer_weight) + 
           (bitrate * bitrate_weight) + 
           (age * age_weight)
```

Higher priority streams are less likely to be moved during consolidation.

#### 4.3.3 Transcoding Distribution

For transcoding workloads:

1. Identify nodes with transcoding capacity
2. Calculate a transcoding score for each node based on current load and capacity
3. Distribute transcoding tasks to maximize parallel processing
4. For high-priority streams, allocate dedicated transcoding resources

## 5. Data Structure Specifications

### 5.1 Node Information

```go
type NodeInfo struct {
    // Basic information
    ID             string
    IP             string
    Port           int
    Role           string // "manager", "edge", "transcoder", etc.
    Region         string
    Version        string
    
    // Resource information
    Capacity       ResourceCapacity
    CurrentLoad    ResourceUsage
    
    // Status information
    Status         string // "healthy", "degraded", "offline"
    LastHeartbeat  time.Time
    JoinTime       time.Time
    
    // Stream information
    Streams        map[string]StreamInfo
    StreamCount    int
    
    // Configuration
    Config         NodeConfig
    
    // Additional metrics
    Metrics        map[string]float64
}
```

### 5.2 Stream Information

```go
type StreamInfo struct {
    // Basic information
    StreamPath       string
    PublisherNodeID  string
    StartTime        time.Time
    
    // Media information
    MediaInfo        MediaDetails
    
    // Statistics
    ViewerCount      int
    BandwidthMbps    float64
    
    // Cluster information
    Priority         int
    ReplicatedTo     []string
    State            string // "active", "inactive", "migrating"
    
    // Synchronization
    VectorClock      map[string]uint64
    LastUpdated      time.Time
    
    // Additional metadata
    Metadata         map[string]string
}
```

### 5.3 Resource Capacity and Usage

```go
type ResourceCapacity struct {
    MaxConcurrentStreams  int
    MaxBandwidthMbps      int
    MaxCPUPercent         float64
    MaxMemoryGB           float64
    TranscodingCapacity   int
    GPUCapacity           int
    
    // Reservation settings
    ReserveCPUPercent     float64
    ReserveMemoryGB       float64
    ReserveBandwidthMbps  int
}

type ResourceUsage struct {
    ConcurrentStreams   int
    BandwidthMbps       float64
    CPUPercent          float64
    MemoryGB            float64
    TranscodingLoad     float64
    GPUUtilization      float64
    DiskIOPS            int
    NetworkLatencyMs    map[string]float64
    
    // Historical data
    HistoricalLoad      []ResourceSnapshot
}

type ResourceSnapshot struct {
    Timestamp     time.Time
    CPUPercent    float64
    MemoryGB      float64
    BandwidthMbps float64
    StreamCount   int
}
```

## 6. API Specifications

### 6.1 REST API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/cluster/api/nodes` | GET | List all nodes in the cluster |
| `/cluster/api/nodes/:id` | GET | Get detailed information about a node |
| `/cluster/api/nodes/:id` | PUT | Update node configuration |
| `/cluster/api/nodes/:id` | DELETE | Remove a node from the cluster |
| `/cluster/api/streams` | GET | List all streams in the cluster |
| `/cluster/api/streams/:path` | GET | Get detailed information about a stream |
| `/cluster/api/balance` | POST | Trigger manual load balancing |
| `/cluster/api/stats` | GET | Get cluster-wide statistics |
| `/cluster/api/topology` | GET | Get cluster topology information |

### 6.2 gRPC Service

```protobuf
service ClasterService {
    // Node management
    rpc RegisterNode(RegisterNodeRequest) returns (RegisterNodeResponse);
    rpc UpdateNodeStatus(UpdateNodeStatusRequest) returns (UpdateNodeStatusResponse);
    rpc GetNodeInfo(GetNodeInfoRequest) returns (GetNodeInfoResponse);
    rpc ListNodes(ListNodesRequest) returns (ListNodesResponse);
    
    // Stream management
    rpc RegisterStream(RegisterStreamRequest) returns (RegisterStreamResponse);
    rpc UpdateStreamInfo(UpdateStreamInfoRequest) returns (UpdateStreamInfoResponse);
    rpc GetStreamInfo(GetStreamInfoRequest) returns (GetStreamInfoResponse);
    rpc ListStreams(ListStreamsRequest) returns (ListStreamsResponse);
    
    // Load balancing
    rpc ReportLoad(ReportLoadRequest) returns (ReportLoadResponse);
    rpc GetLoadBalancingDecision(GetLoadBalancingDecisionRequest) returns (GetLoadBalancingDecisionResponse);
    rpc ConfirmLoadBalancingAction(ConfirmLoadBalancingActionRequest) returns (ConfirmLoadBalancingActionResponse);
    
    // Stream synchronization
    rpc SyncStreams(SyncStreamsRequest) returns (SyncStreamsResponse);
    rpc GetBloomFilter(GetBloomFilterRequest) returns (GetBloomFilterResponse);
    rpc ReportStreamInconsistency(ReportStreamInconsistencyRequest) returns (ReportStreamInconsistencyResponse);
    
    // Monitoring
    rpc GetClusterStats(GetClusterStatsRequest) returns (GetClusterStatsResponse);
    rpc GetClusterAlerts(GetClusterAlertsRequest) returns (GetClusterAlertsResponse);
}
```

## 7. Security Considerations

### 7.1 Authentication

All nodes in the cluster must authenticate using:
- Shared secret key
- TLS client certificates
- Token-based authentication (JWT)

### 7.2 Authorization

- Role-based access control for API endpoints
- Node permissions based on assigned role
- Command authorization based on node role

### 7.3 Secure Communication

- All inter-node communication encrypted using TLS
- QUIC protocol with encryption for data transfer
- Periodic key rotation for shared secrets

### 7.4 Data Protection

- Stream metadata encrypted at rest
- Sensitive configuration values encrypted
- Access logs for administrative actions

## 8. Extensibility

### 8.1 Plugin System

The Cluster plugin itself supports a modular architecture with extension points:

- Custom load balancing algorithms
- Custom stream prioritization strategies
- Additional monitoring metrics
- Custom health check implementations
- Event hooks for major system events

### 8.2 Custom Algorithms

Users can implement and register custom algorithms for:

- Load balancing
- Stream placement
- Resource optimization
- Failure detection
- Stream synchronization

## 9. Monitoring and Debugging

### 9.1 Logging

- Structured JSON logging
- Configurable log levels
- Component-based logging
- Log rotation and archiving

### 9.2 Metrics

- Prometheus-compatible metrics
- Grafana dashboard templates
- Custom metrics for specific deployments
- Historical metrics storage

### 9.3 Debugging Tools

- Protocol analyzers for CCP, SSP, and LBEP
- State dumping for troubleshooting
- Simulation mode for testing configuration changes
- Distributed tracing integration

## 10. Deployment Considerations

### 10.1 Minimum Cluster Size

- Minimum of 3 nodes recommended for high availability
- Single manager node possible for testing/development
- Maximum practical cluster size of 100 nodes with standard configuration

### 10.2 Network Requirements

- Low-latency connections between nodes (<50ms preferred)
- Consistent bandwidth between nodes
- NAT traversal capabilities for nodes in different networks
- UDP port accessibility for QUIC protocol

### 10.3 Database Considerations

- Local state stored in embedded database
- Option for external PostgreSQL for large clusters
- Regular database backups recommended
- Automatic database migration during upgrades 