package networking

import (
	"context"
	"fmt"
	awssdk "github.com/aws/aws-sdk-go/aws"
	ec2sdk "github.com/aws/aws-sdk-go/service/ec2"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/cache"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/algorithm"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/aws/services"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/k8s"
	"strings"
	"sync"
	"time"
)

const (
	defaultNodeENIInfoCacheTTL = 10 * time.Minute
)

// NodeENIInfoResolver is responsible for resolve the AWS VPC ENI that supports node network.
type NodeENIInfoResolver interface {
	Resolve(ctx context.Context, nodes []*corev1.Node) (map[types.NamespacedName]ENIInfo, error)
}

// NewDefaultNodeENIInfoResolver constructs new defaultNodeENIInfoResolver.
func NewDefaultNodeENIInfoResolver(nodeInfoProvider NodeInfoProvider, ec2Client services.EC2, vpcID string, logger logr.Logger) *defaultNodeENIInfoResolver {
	return &defaultNodeENIInfoResolver{
		nodeInfoProvider:      nodeInfoProvider,
		ec2Client:             ec2Client,
		vpcID:                 vpcID,
		logger:                logger,
		nodeENIInfoCache:      cache.NewExpiring(),
		nodeENIInfoCacheMutex: sync.RWMutex{},
		nodeENIInfoCacheTTL:   defaultNodeENIInfoCacheTTL,
	}
}

var _ NodeENIInfoResolver = &defaultNodeENIInfoResolver{}

// default implementation for NodeENIInfoResolver.
type defaultNodeENIInfoResolver struct {
	// nodeInfoProvider
	nodeInfoProvider NodeInfoProvider
	ec2Client        services.EC2
	vpcID            string

	// logger
	logger logr.Logger

	nodeENIInfoCache      *cache.Expiring
	nodeENIInfoCacheMutex sync.RWMutex
	nodeENIInfoCacheTTL   time.Duration
}

func (r *defaultNodeENIInfoResolver) Resolve(ctx context.Context, nodes []*corev1.Node) (map[types.NamespacedName]ENIInfo, error) {
	eniInfoByNodeKey := r.fetchENIInfosFromCache(nodes)
	nodesWithoutENIInfo := computeNodesWithoutENIInfo(nodes, eniInfoByNodeKey)
	if len(nodesWithoutENIInfo) > 0 {
		if strings.Contains(nodesWithoutENIInfo[0].Name, "hyperpod") {
			res := make(map[types.NamespacedName]ENIInfo)
			for _, node := range nodesWithoutENIInfo {
				namespacedName := types.NamespacedName{
					Namespace: node.Namespace,
					Name:      node.Name,
				}
				addresses := node.Status.Addresses
				eniByID, err := r.getENIMappingViaDescribe(ctx, []string{addresses[0].Address}, "addresses.private-ip-address")
				if err != nil {
					return nil, err
				}
				for _, eni := range eniByID {
					eniInfo := buildENIInfoViaENI(eni)
					res[namespacedName] = eniInfo
				}
			}
			r.logger.Info(fmt.Sprintf("[HP] Hack nodeENI Info: [%s]", res))
			return res, nil
		}

		eniInfoByNodeKeyViaLookup, err := r.resolveViaInstanceID(ctx, nodesWithoutENIInfo)
		if err != nil {
			return nil, err
		}
		r.saveENIInfosToCache(nodesWithoutENIInfo, eniInfoByNodeKeyViaLookup)
		for nodeKey, eniInfo := range eniInfoByNodeKeyViaLookup {
			eniInfoByNodeKey[nodeKey] = eniInfo
		}
		nodesWithoutENIInfo = computeNodesWithoutENIInfo(nodesWithoutENIInfo, eniInfoByNodeKeyViaLookup)
	}

	if len(nodesWithoutENIInfo) > 0 {
		unresolvedNodeKeys := make([]types.NamespacedName, 0, len(nodesWithoutENIInfo))
		for _, node := range nodesWithoutENIInfo {
			unresolvedNodeKeys = append(unresolvedNodeKeys, k8s.NamespacedName(node))
		}
		return nil, errors.Errorf("cannot resolve node ENI for nodes: %v", unresolvedNodeKeys)
	}
	return eniInfoByNodeKey, nil
}

func (r *defaultNodeENIInfoResolver) getENIMappingViaDescribe(ctx context.Context, nodeIPs []string, ipAddressFilterKey string) (map[string]*ec2sdk.NetworkInterface, error) {
	nodeIPChunks := algorithm.ChunkStrings(nodeIPs, 199)
	eniByID := make(map[string]*ec2sdk.NetworkInterface)
	for _, podIPChunk := range nodeIPChunks {
		req := &ec2sdk.DescribeNetworkInterfacesInput{
			Filters: []*ec2sdk.Filter{
				{
					Name:   awssdk.String("vpc-id"),
					Values: awssdk.StringSlice([]string{r.vpcID}),
				},
				{
					Name:   awssdk.String(ipAddressFilterKey),
					Values: awssdk.StringSlice(podIPChunk),
				},
			},
		}
		r.logger.Info(fmt.Sprintf("[HP] NodeEniInfoResolver describeNetworkInterfaces: [%v]", req))
		enis, err := r.ec2Client.DescribeNetworkInterfacesAsList(ctx, req)
		r.logger.Info(fmt.Sprintf("[HP] NodeEniInfoResolver enis: [%v], err: [%v]", enis, err))
		if err != nil {
			return nil, err
		}
		for _, eni := range enis {
			eniID := awssdk.StringValue(eni.NetworkInterfaceId)
			eniByID[eniID] = eni
		}
	}
	return eniByID, nil
}

type nodeENIInfoCacheKey struct {
	// Node's key
	nodeKey types.NamespacedName
	// Node's UID.
	// Note: we assume node's eni haven't changed as long as node UID is same.
	nodeUID types.UID
}

func (r *defaultNodeENIInfoResolver) fetchENIInfosFromCache(nodes []*corev1.Node) map[types.NamespacedName]ENIInfo {
	r.nodeENIInfoCacheMutex.RLock()
	defer r.nodeENIInfoCacheMutex.RUnlock()

	eniInfoByNodeKey := make(map[types.NamespacedName]ENIInfo)
	for _, node := range nodes {
		cacheKey := computeNodeENIInfoCacheKey(node)
		if rawCacheItem, exists := r.nodeENIInfoCache.Get(cacheKey); exists {
			eniInfo := rawCacheItem.(ENIInfo)
			nodeKey := k8s.NamespacedName(node)
			eniInfoByNodeKey[nodeKey] = eniInfo
		}
	}
	return eniInfoByNodeKey
}

func (r *defaultNodeENIInfoResolver) saveENIInfosToCache(nodes []*corev1.Node, eniInfoByNodeKey map[types.NamespacedName]ENIInfo) {
	r.nodeENIInfoCacheMutex.Lock()
	defer r.nodeENIInfoCacheMutex.Unlock()

	for _, node := range nodes {
		nodeKey := k8s.NamespacedName(node)
		if eniInfo, exists := eniInfoByNodeKey[nodeKey]; exists {
			cacheKey := computeNodeENIInfoCacheKey(node)
			r.nodeENIInfoCache.Set(cacheKey, eniInfo, r.nodeENIInfoCacheTTL)
		}
	}
}

func (r *defaultNodeENIInfoResolver) resolveViaInstanceID(ctx context.Context, nodes []*corev1.Node) (map[types.NamespacedName]ENIInfo, error) {
	nodeInstanceByNodeKey, err := r.nodeInfoProvider.FetchNodeInstances(ctx, nodes)
	if err != nil {
		return nil, err
	}
	eniInfoByNodeKey := make(map[types.NamespacedName]ENIInfo, len(nodeInstanceByNodeKey))
	for nodeKey, nodeInstance := range nodeInstanceByNodeKey {
		primaryENI, err := findInstancePrimaryENI(nodeInstance.NetworkInterfaces)
		if err != nil {
			return nil, err
		}
		eniInfo := buildENIInfoViaInstanceENI(primaryENI)
		eniInfoByNodeKey[nodeKey] = eniInfo
	}
	return eniInfoByNodeKey, nil
}

// findInstancePrimaryENI returns the primary ENI among list of eni on an EC2 instance
func findInstancePrimaryENI(enis []*ec2sdk.InstanceNetworkInterface) (*ec2sdk.InstanceNetworkInterface, error) {
	for _, eni := range enis {
		if awssdk.Int64Value(eni.Attachment.DeviceIndex) == 0 {
			return eni, nil
		}
	}
	return nil, errors.Errorf("[this should never happen] no primary ENI found")
}

// computeNodeENIInfoCacheKey computes the cacheKey for node's ENIInfo cache.
func computeNodeENIInfoCacheKey(node *corev1.Node) nodeENIInfoCacheKey {
	return nodeENIInfoCacheKey{
		nodeKey: k8s.NamespacedName(node),
		nodeUID: node.UID,
	}
}

// computeNodesWithoutENIInfo computes nodes that don't have resolvedENIInfo.
func computeNodesWithoutENIInfo(nodes []*corev1.Node, eniInfoByNodeKey map[types.NamespacedName]ENIInfo) []*corev1.Node {
	unresolvedNodes := make([]*corev1.Node, 0, len(nodes)-len(eniInfoByNodeKey))
	for _, node := range nodes {
		nodeKey := k8s.NamespacedName(node)
		if _, ok := eniInfoByNodeKey[nodeKey]; !ok {
			unresolvedNodes = append(unresolvedNodes, node)
		}
	}
	return unresolvedNodes
}
