* block verify with context (ShouldVerifyWithContext && VerifyWithContext)
  * check if block must have context (https://github.com/ava-labs/xsvm/blob/master/execute/expects_context.go#L13)
  * skip verify if bootstrapping (getting rollback of validator state may slow down node)
* track balance exported to a given region and don't let more come back
  * https://github.com/ava-labs/xsvm/blob/cbeff25ecbe3330cb3dc6effe065b1541c3bb77b/state/storage.go#L133-L200
* don't allow for transfering balances from a chain through another (ONLY direct)
* async generate message signature and make queryable over P2P by anyone
* allow actions to define a "warpable" payload: https://github.com/ava-labs/xsvm/blob/cbeff25ecbe3330cb3dc6effe065b1541c3bb77b/execute/tx.go#L88-L95
* verify warp signature: https://github.com/ava-labs/xsvm/blob/cbeff25ecbe3330cb3dc6effe065b1541c3bb77b/execute/tx.go#L159-L181
* fetch signatures from validators and aggregate: https://github.com/ava-labs/xsvm/blob/cbeff25ecbe3330cb3dc6effe065b1541c3bb77b/cmd/issue/importtx/cmd.go
  * add as mode on node and expose via API for anyone to call (return
    signatures, not aggregate signature -> leave to CLI to submit something
    valid and reduce compute on API)
* create shared hypersdk action that can route to an internal action?

future work
* verify multi-signature async