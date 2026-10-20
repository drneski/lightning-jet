const parallel = require('run-parallel');
const deasync = require('deasync');
const logger = require('../api/logger');

const round = n => Math.round(n);

module.exports = {
  walletBalance(lndClient) {
    let error, response, done;
    lndClient.walletBalance({}, (err, resp) => {
      error = err;
      response = resp;
      done = true;
    })
    deasync.loopWhile(function() { return done === undefined });
    return { error: error, response: response };
  },
  closeChannel(lndClient, chanId, fee, force = false, cbk) {
    const chanInfo = module.exports.getChanInfo(lndClient, chanId);
    if (chanInfo.error) return cbk({ error: 'error fetching channel info: ' + chanInfo.error.details });
    const chanPoint = chanInfo.chan.chan_point;
    const txid = chanPoint.split(':')[0];
    const index = parseInt(chanPoint.split(':')[1]);
    if (txid === undefined) return cbk({ error: 'could not identify funding tx id' });
    if (index === undefined) return cbk({ error: 'could not identify funding tx index' });

    const req = { 
      channel_point: {
        funding_txid_str: txid,
        output_index: index
      }, 
      force: force
    }
    if (fee !== undefined) req.sat_per_vbyte = fee;
    let error, response, done;
    const call = lndClient.closeChannel(req);
    call.on('data', function(resp) {
      response = resp;
      done = true;
    })
    call.on('status', function(status) {
      done = true;
    })
    call.on('end', function() {
      done = true;
    })
    call.on('error', function(err) {
      error = err;
      done = true;
    })
    deasync.loopWhile(function() { return done === undefined });
    return { error: error, response: response };
  },
  getChanInfo(lndClient, chanId) {
    let chan, error, done;
    lndClient.getChanInfo({chan_id: chanId}, (err, response) => {
      error = err;
      chan = response;
      done = true;
    })
    deasync.loopWhile(function() { return done === undefined });
    return { error: error, chan: chan };
  },
  listPaymentsSync(lndClient, offset = 0, max = 100) {
    const pref = 'listPaymentsSync:';
    if (!lndClient) throw new Error(pref + ' lndClient missing');
    const req = {
      index_offset: offset,
      max_payments: max
    }
    let done = false;
    let response;
    let error;
    lndClient.listPayments(req, (err, resp) => {
      error = err;
      response = resp;
      done = true;
    })
    while(!done) {
      require('deasync').runLoopOnce();
    }
    return {error, response};
  },
  // lists forwards from a start date
  // <timestamp> - starting time for the query as unix timestamp
  // [max] - max forwards to return, default 100
  listForwardsSync(lndClient, timestamp, offset = 0, max = 100) {
    const pref = 'listForwardsSync:';
    if (!lndClient) throw new Error(pref + ' lndClient missing');
    if (!timestamp) throw new Error(pref + ' timestamp missing');
    const req = {
      start_time: timestamp,
      index_offset: offset,
      num_max_events: max
    }
    let done = false;
    let response;
    let error;
    lndClient.forwardingHistory(req, (err, resp) => {
      error = err;
      response = resp;
      done = true;
    })
    while(!done) {
      require('deasync').runLoopOnce();
    }
    return {error, response};
  },
  forwardHistorySync: function(lndClient, secs = 5 * 60, max = 1000) {
    if (!lndClient) throw new Error('forwardHistorySync: need lndClient');
    let done = false;
    let response;
    let error;
    try {
      const start = Math.floor(+new Date() / 1000) - secs;
      const req = {start_time:start, num_max_events:max};
      lndClient.forwardingHistory(req, (err, resp) => {
        if (err) error = err; else response = resp;
        done = true;
      })
    } catch(err) {
      error = err;
      done = true;
    }
    while(!done) {
      require('deasync').runLoopOnce();
    }
    return {error, events:response && response.forwarding_events};
  },
  // return true if lnd is alive, false otherwise
  isLndAlive: function(lndClient) {
    logger.debug('lnd alive check');
    if (!lndClient) throw new Error('isLndAlive: need lndClient');
    // do a simple ping (perhaps replace it with getVersion)
    const { getInfoSync } = module.exports;
    try {
      const info = getInfoSync(lndClient);
      return info !== undefined;
    } catch(err) {
      logger.debug('error calling getInfo: ' + err.message);
      return false;
    }
  },
  withCommas: function(s) {
    return (s) ? s.toString().replace(/\B(?<!\.\d*)(?=(\d{3})+(?!\d))/g, ",") : s;
  },
  stuckHtlcsSync: function(lndClient) {
    let data;
    let error;
    let done;
    lndClient.listChannels({}, function(err, response) {
      error = err;
      data = response;
      done = true;
    })
    deasync.loopWhile(() => !done);
    if (error) throw new Error(error);

    // build a map of channel ids to pubkeys
    let map = {};
    data.channels.forEach(c => {
      map[c.chan_id] = c.remote_pubkey;
    })

    let htlcs = [];
    data.channels.forEach(c => {
      if (c.pending_htlcs.length === 0) return;
      // add peer ids for forwarding channels
      c.pending_htlcs.forEach(h => {
        if (h.forwarding_channel !== '0') h.forwarding_peer = map[h.forwarding_channel];
      })
      htlcs.push({
        id: c.chan_id,
        peer: c.remote_pubkey,
        htlcs: c.pending_htlcs
      })
    })
    return htlcs;
  },
  sendMessageToNode: function(routerRpc, node, message) {
    sendMessage();
    async function sendMessage() {
      let req = {
        dest: node,
        amp: true,
        timeout_seconds: 60
      }
      for await (const payment of routerRpc.sendPaymentV2(req)) {
        console.log('payment:', payment);
      }
    }
  },
  listPendingChannelsSync: function(lndClient) {
    let channels;
    let error;
    let done;
    lndClient.pendingChannels({}, function(err, response) {
      error = err;
      channels = response;
      done = true;
    })
    while(done === undefined) {
      require('deasync').runLoopOnce();
    }
    if (error) throw new Error(error);
    return channels;
  },
  htlcHistorySync: function(lndClient, days = 7) {
    let currTime = Math.floor(+new Date() / 1000);
    let history;
    lndClient.forwardingHistory({
      start_time: currTime - days * 24 * 60 * 60,
      num_max_events: 10000
    }, (err, response) => {
      if (err) {
        return console.error('htlcHistorySync: ' + err);
      }
      history = response;
    })
    while(history === undefined) {
      require('deasync').runLoopOnce();
    }

    // aggregate
    let mapIn = {};
    let mapOut = {}
    history.forwarding_events.forEach(h => {
      let sum = mapIn[h.chan_id_in] || 0;
      mapIn[h.chan_id_in] = sum + parseInt(h.amt_in);
      sum = mapOut[h.chan_id_out] || 0;
      mapOut[h.chan_id_out] = sum + parseInt(h.amt_out);
    })

    let peers = module.exports.listPeersMapSync(lndClient);
    let channels = {};
    module.exports.listChannelsSync(lndClient).forEach(c => {
      channels[c.chan_id] = c;
    })

    let fees = module.exports.listFeesSync(lndClient);
    let feeMap = {};
    fees.forEach(f => feeMap[f.chan] = f);

    //console.log(channels);
    //console.log(mapIn);
    const computeMargin = (local, remote) => Math.round(local.base/1000 + local.rate - (remote.base/1000 + remote.rate));
    let unknown = [];
    let inPeers = [];
    Object.keys(mapIn).forEach(n => {
      if (!channels[n]) {
        unknown.push(n);
        //console.log('couldnt find channel data for', n);
        return;
      }
      let entry = {
        active: channels[n].active,
        id: channels[n].chan_id,
        peer: channels[n].remote_pubkey,
        name: peers[channels[n].remote_pubkey].name,
        sum: mapIn[n],
        lifetime: channels[n].lifetime,
        capacity: channels[n].capacity
      }
      let ppm = feeMap[n] && feeMap[n].local && feeMap[n].local.rate;
      if (ppm == undefined) {
        console.warn('couldnt locate fees for', n);
      } else {
        entry.ppm = ppm;
        entry.margin = computeMargin(feeMap[n].local, feeMap[n].remote);
      }
      inPeers.push(entry);
    })
    inPeers.sort(function(a, b) {
      return b.sum - a.sum;
    })
    let outPeers = [];
    Object.keys(mapOut).forEach(n => {
      if (!channels[n]) {
        unknown.push(n);
        //console.log('couldnt find channel data for', n);
        return;
      }
      let entry = {
        active: channels[n].active,
        id: channels[n].chan_id,
        peer: channels[n].remote_pubkey,
        name: peers[channels[n].remote_pubkey].name,
        sum: mapOut[n],
        lifetime: channels[n].lifetime,
        capacity: channels[n].capacity
      }
      let ppm = feeMap[n] && feeMap[n].local && feeMap[n].local.rate;
      if (ppm == undefined) {
        console.warn('couldnt locate fees for', n);
      } else {
        entry.ppm = ppm;
        entry.margin = computeMargin(feeMap[n].local, feeMap[n].remote);
      }
      outPeers.push(entry);
    })
    outPeers.sort(function(a, b) {
      return b.sum - a.sum;
    })
    unknown = unknown.filter(function(elem, pos) {
      return unknown.indexOf(elem) === pos;
    })

    // generate balance map
    let inMap = {};
    let outMap = {};
    let balanceMap = {};
    let inSum = 0;
    let outSum = 0;
    inPeers.forEach(h => { inMap[h.id] = h; inSum += h.sum});
    outPeers.forEach(h => { outMap[h.id] = h; outSum += h.sum});
    Object.keys(channels).forEach(k => {
      if (inMap[k]) {
        if (outMap[k]) {
          balanceMap[k] = round(100 * inMap[k].sum / (inMap[k].sum + outMap[k].sum));
        } else {
          balanceMap[k] = 100;
        }
      } else if (outMap[k]) {
        if (inMap[k]) {
          balanceMap[k] = round(100 * inMap[k].sum / (inMap[k].sum + outMap[k].sum));
        } else {
          balanceMap[k] = 0;
        }
      }
    })
    inPeers.forEach(h => { 
      h.p = round(100 * h.sum / inSum); 
      h.d = balanceMap[h.id];
    })
    outPeers.forEach(h => { 
      h.p = round(100 * h.sum / outSum);
      h.d = 100 - balanceMap[h.id]
    })

    return {inbound: inPeers, outbound: outPeers, unknown: unknown};
  },
  getNodeFeeSync: function(lndClient, node) {
    // not the most optimal implementation, rethink
    let fees = module.exports.listFeesSync(lndClient);
    let fee;
    fees.forEach(f => {
      if (f.id === node) { fee = f; return; }
    })
    return fee && fee.remote;
  },
  listFeesSync: function(lndClient, chans) {
    let fees, done, error;
    try {
      module.exports.listFees(lndClient, chans, function(result) {
        fees = result;
        done = true;
      })
    } catch(err) {
      error = err;
      done = true;
    }
    while(done === undefined) {
      require('deasync').runLoopOnce();
    }
    if (error) throw new Error(error);
    else return fees;
  },
  listFees: function(lndClient, chans, callback) {
    const pref = 'listFees:';
    let info = module.exports.getInfoSync(lndClient);
    let peers = module.exports.listPeersMapSync(lndClient);
    //console.log(peers);
    let ids = chans;
    if (!ids) {
      ids = [];
      let channels = module.exports.listChannelsSync(lndClient);
      channels.forEach(c => ids.push({peer: c.remote_pubkey, chan: c.chan_id}));
    }
    let calls = [];
    ids.forEach(id => {
      calls.push(function(cb) {
        lndClient.getChanInfo({chan_id: id.chan}, (err, response) => {
          if (err) {
            // report the error, but make sure to continue with other channels
            logger.debug(pref, 'chan ' + id.chan + ':', err.message);
            return cb(null);
          }
          response.peer = id.peer;
          return cb(null, response);
        })
      })
    })
    parallel(calls, function (err, results) {
      if (err) {
        console.log(err);
        return callback(null);
      }

      //console.log(results);
      let fees = [];
      results.forEach(r => {
        if (!r) return; // something went wrong with getChanInfo
        let fee = {
          chan: r.channel_id,
          id: r.peer,
          name: peers[r.peer] ? peers[r.peer].name : r.peer,  // no node announcement
        }
        if (!r.node1_policy || !r.node2_policy) {
          // likely that a channel has been added but fees haven't yet
          // been propagated by gossip
          return console.warn('undefined fee policy:', r);
        }
        if (r.node1_pub === info.identity_pubkey) {
          fee.local = {
            base: parseInt(r.node1_policy.fee_base_msat),
            rate: parseInt(r.node1_policy.fee_rate_milli_msat)
          }
          fee.remote = {
            base: parseInt(r.node2_policy.fee_base_msat),
            rate: parseInt(r.node2_policy.fee_rate_milli_msat)
          }
        } else {
          fee.local = {
            base: parseInt(r.node2_policy.fee_base_msat),
            rate: parseInt(r.node2_policy.fee_rate_milli_msat)
          }
          fee.remote = {
            base: parseInt(r.node1_policy.fee_base_msat),
            rate: parseInt(r.node1_policy.fee_rate_milli_msat)
          }
        }
        fees.push(fee);
      })
      return callback(fees);
    })
  },
  getInfoSync: function(lndClient) {
    logger.debug('getting node info');
    var info, done, error;
    lndClient.getInfo({}, (err, resp) => {
      error = err;
      info = resp;
      done = true;
    })
    deasync.loopWhile(() => !done);

    if (error) throw new Error(error);
    if (!info) throw new Error('error getting node info');
    return info;
  },
  listPeersMapSync: function(lndClient) {
    let map;
    let peers = module.exports.listPeersSync(lndClient);
    if (peers) {
      map = {};
      peers.forEach(p => {
        map[p.id] = p;
      })
    }
    return map;
  },
  listPeersSync: function(lndClient) {
    const pref = 'listPeersSync:';
    let channels = module.exports.listChannelsSync(lndClient);
    let peerIds = [];
    channels.forEach(c => peerIds.push(c.remote_pubkey));
    let peerInfo = {};
    module.exports.getNodesInfoSync(lndClient, peerIds).forEach(p => {
      // !p means an error getting peer info, already reported in
      // getNodesInfoSync; continue
      if (p) peerInfo[p.node.pub_key] = p;
    })
    let peers = [];
    channels.forEach(c => {
      // fix funkiness in the alias that screws up the output
      // IMPORTANT: this is temporary, needs to be removed
      if (!peerInfo[c.remote_pubkey]) {
        return logger.debug(pref, 'couldnt find info for', c.remote_pubkey);
      }
      let name = peerInfo[c.remote_pubkey].node.alias;
      name = module.exports.removeEmojis(name);  // get rid of emojis to avoid skewed tables

      peers.push({
        id: c.remote_pubkey,
        name: name,
        out: c.local_balance,
        in: c.remote_balance,
        active: c.active
      })
    })
    return peers;
  },
  listChannelsMapSync: function(lndClient) {
    let map;
    let channels = module.exports.listChannelsSync(lndClient);
    if (channels) {
      map = {};
      channels.forEach(c => {
        map[c.remote_pubkey] = c;
      })
    }
    return map;
  },
  listChannelsSync: function(lndClient) {
    let channels;
    let error;
    let done;
    lndClient.listChannels({}, (err, response) => {
      error = err;
      channels = response && response.channels;
      done = true;
    })
    deasync.loopWhile(() => !done);
    if (error) throw new Error(error);
    return channels;
  },
  getNodeInfoSync: function(lndClient, id) {
    let done, error, info;
    lndClient.getNodeInfo({pub_key: id}, (err, response) => {
      error = err;
      info = response;
      done = true;
    })
    deasync.loopWhile(() => !done);
    return {error, info};
  },
  getNodesInfo: function(lndClient, nodes, callback) {
    let calls = [];
    nodes.forEach(n => {
      calls.push(function(cb) {
        lndClient.getNodeInfo({pub_key: n}, (err, response) => {
          if (err) {
            logger.warn('getNodesInfo: error locating', n);
            logger.debug('getNodesInfo:', n, err);
            return cb(null, null);
          }
          return cb(null, response);
        })
      })
    })

    parallel(calls, function (err, results) {
      // the results array will equal ['one','two'] even though
      // the second function had a shorter timeout.
      if (err) {
        console.log(err);
        return callback(null);
      }
      return callback(results);
    })
  },
  getNodesInfoSync: function(lndClient, nodes) {
    if (!nodes || nodes.length === 0) {
      return nodes;
    }
    let info;
    module.exports.getNodesInfo(lndClient, nodes, (result) => {
      info = result;
    })
    deasync.loopWhile(() => !info);
    return info;
  },
  removeEmojis: function(str) {
    const {isEmoji} = require('../api/constants');
    return str.replace(isEmoji, String()).trim();
  },
  closeLndHandle(lndClient) {
    if (lndClient && typeof lndClient.close === 'function') {
      logger.debug('closing lnd handle');
      lndClient.close();
    }
  }
}
