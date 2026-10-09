const deasync = require('deasync');
const {getInfoSync} = require('../lnd-api/utils');

const toBytes = id => Buffer.from(id, 'hex').reverse();

module.exports = {
  // returns true if successful, throws an error otherwise
  updateChannelSync: function(lndClient, req) {
    if (!req.chan) throw new Error('channel is missing');
    if (req.base === undefined && req.ppm === undefined) throw new Error('either base or ppm need to be provided');

    // get channel info
    let chan, error, done;
    lndClient.getChanInfo({chan_id: req.chan}, (err, resp) => {
      error = err;
      chan = resp;
      done = true;
    })
    deasync.loopWhile(() => !done);

    if (error) throw new Error('error getting channel info: ' + error.toString());
    
    let nodeInfo;
    try {
     nodeInfo = getInfoSync(lndClient);
    } catch(err) {
      throw new Error('error getting node info: ' + err.toString());
    }

    // updatechannelpolicy sets the whole policy: base fee, fee rate and time lock
    // delta that aren't passed are zeroed out (or set to whatever is passed),
    // not kept. carry over the channel's current values for anything the caller
    // didn't ask to change, so that e.g. setting the base fee doesn't zero the ppm.
    let policy = (nodeInfo.identity_pubkey === chan.node1_pub) ? chan.node1_policy : chan.node2_policy;
    if (!policy) throw new Error('current policy for the channel is unknown, try again later');

    let tokens = chan.chan_point.split(':');
    let cpoint = {
      funding_txid_str: tokens[0],
      output_index: parseInt(tokens[1]),
    };
    let grpc = {
      chan_point: cpoint,
      base_fee_msat: (req.base !== undefined) ? req.base : policy.fee_base_msat,
      fee_rate: ((req.ppm !== undefined) ? req.ppm : Number(policy.fee_rate_milli_msat)) / 1000000,
      time_lock_delta: policy.time_lock_delta
    };
    //console.log(grpc);

    done = false;
    error = undefined;
    lndClient.updateChannelPolicy(grpc, (err, resp) => {
      error = err;
      done = true;
    })
    deasync.loopWhile(() => !done);
 
    if (error) throw new Error('error updating channel: ' + error.toString());
    return true;
  }
}
