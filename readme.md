# Distributed rate-limiting POC

This is a proof of concept that demonstrates a scalable algorithm/technique for distributed rate-limiting. Lets first explain what I mean by distributed rate-limiting. Lets say we are running some sort of proxy, network filter or other application which is distributed over multiple nodes. On this service we want to apply rate limiting to the total traffic received across all nodes.

Now the issue with this task is that a node doesn't "know" what amounts of traffic the other nodes have seen. So we need to make our nodes coordinate somehow. The challenge is that there is always a tradeoff between performance and accuracy. Lets look at a few ways we could solve this (not an exhaustive list):

* Create a central database/node which is responsible for maintaining a count. Every time a node gets a request we check in with this central node to see if we may allow a request. This approach is 100% accurate, however it doesn't scale very well. Since every request must wait for a round trip to this central node it has high latency and we are effectively limited by the performance of this central node. Not to mention the single point of failure.
* Let every node maintain its own counter, every time we get a request we send a message to every other node so they can increment their local counters. This method is slightly less accurate but still very good. The big downside is the total amount of traffic we need to send. Lets assume we have 5 nodes that send an update every second. That means that we will have N*(N-1) amounts of updates per second, so for 5 nodes 20 updates. But if we double the amount of nodes to 10 we get 90 updates per second. Which means this method doesn't scale.
* If traffic was always perfectly balanced between all nodes you could just split the rate limit threshold by the amount of nodes. The upside is that you only need communication between nodes when a node has been added or removed to update the limits and thus would scale very well, however, in practice traffic is rarely perfectly balanced leading to inaccurate limiting in both directions.

This method strikes a balance between performance and accuracy. It can scale without additional performance, however, large clusters have worse accuracy than small clusters. But the amount of accuracy seems acceptable.

## How it works

The way this algorithm/method works is by arranging the nodes of our cluster into a [binary heap](https://en.wikipedia.org/wiki/Binary_heap). Normally heaps are local data structures, but in our case every node of the heap is an actual node in our service. Two important properties of the binary heap is that every node has at most 3 edges and that it is "complete", that is, all levels of the tree, except possibly the last one (deepest) are fully filled.

Next, we make every node keep track of 4 numbers for every counter, we will call these 'toParent', 'toLeft', 'toRight' and 'localCount'. Every time a request is received on a node we will increment all of these counters. At a fixed interval 'Δs'(Delta sync) each node will send their parent node the value in 'toParent', send 'toLeft' to their left node, and send 'toRight' to their right node. After sending the counters, all but the 'localCount' are reset to 0. When a node receives an update, it checks from which node it came and add the value to all counters except the one for the sending node. So for example when we receive an update from the left node, we update 'toParent', 'toRight', and 'localCount'.

By doing this each node effectively sums all counters of it's sub-tree and sends it to the rest of the tree. Each node can use the 'localCount' to determine if a rate-limit has been reached. Since there are at most 3 nodes connected, this method scales infinitely without impacting performance. The tradeoff is that larger clusters have slower total propagation time from one end of the tree to the other.

Lets make a few calculations to see what our worst case propagation time is.

Vars:
* N = Number of nodes
* D = Depth of heap = Log2(N+1)
* E = Number of edges from Max depth to root = D-1
* Δs = Time in seconds between update packets
* Δt = Network delay between nodes (Avg of all edges)

So since there is a Δs+Δt delay for every edge and we know that there are twice E amount of edges between the bottom left and bottom right nodes in the heap. The non-expanded formula for Worst propagation is:
Pw = 2 * E * (Δs+Δt)
The expanded version is:
Pw = 2 * (Log2(N+1)-1) * (Δs+Δt)

If we assume a 5ms Δt get the following table:
N | Pw(Δs=0.5s) | Pw(Δs=0.1s) | Pw(Δs=0.05s)
--|--|--|--
3 | 1.01s | 0.21s | 0.11s 
10 | 2.48s | 0.51s | 0.27s 
20 | 3.42s | 0.71s | 0.37s 
50 | 4.71s | 0.98s | 0.51s 
100 | 5.71s | 1.19s | 0.62s 
1000 | 9.06s | 1.88s | 0.99s 
5000 | 11.40s | 2.37s | 1.24s 

## TODO/Next steps

A few modifications need to be made to the example code before it is usable:

* Counters are never removed, to save on memory we can implement a periodic goroutine which removes all counters that have not been used in a while.
* Configurable key types. Currently strings are used for both the ratelimiter name and key, but if we can use smaller datatypes like UUID's or enum values, that could save some transfer overhead
* Max packet size should be configurable, usage of jumbo packets can increase the efficiency of transfer, currently limited to 1500 bytes MTU for most networks.
* If the 'toParent', 'toLeft' or 'toRight' value is 0 it shouldn't be sent (small optimization)
* There should be some healthcheck mechanism implemented. So if a node dies the remaining nodes can rearrange.
* Nodes should periodically check for new nodes or removed nodes in config. 
* The current code skips error checks in a few places and used panic in others instead of gracefully handling the situation.