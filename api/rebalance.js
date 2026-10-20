// rebalance built on top of https://github.com/alexbosworth/balanceofsatoshis
//
// calls 'bos rebalance' in a loop, until the target amount is met or until all
// possible routes is exhausted.  dynamically builds a list of nodes to avoid
// based on candidate routes.  once it encounters a candidate route where max ppm
// exceeds the argument, it excludes the most expensive route, or the corresponding hop.
//
// aggregates stats for nodes that it encounters. the aggregated stats make it
// easier to find candidate nodes.  e.g.:
// {
//    "name": "LightTheClassic",
//    "id": "02c29b89b2121b2c1fa2e5422bc70e0bb7ae7326c7a9d2b796ed6b89cdc5a2871b",
//    "ppms": "189,189,189,189",
//    "avg_ppm": 189,
//    "max_ppm": 189,
//    "channels": 67,
//    "updated_h_ago": 14
// }
// where ppms is a list of ppms for nodes that are encountered during routing.
//

const config = require('./config');
const tags = require('./tags');
const date = require('date-and-time');
const constants = require('./constants');
const logger = require('./logger');
const importLazy = require('import-lazy')(require);
const lndClient = importLazy('./connect');
const {getNodesInfoSync} = require('../lnd-api/utils');
const {recordRebalance} = require('../db/utils');
const {recordRebalanceFailure} = require('../db/utils');
const {recordRebalanceAvoid} = require('../db/utils');
const {listRebalanceAvoidSync} = require('../db/utils');
const {recordActiveRebalanceSync} = require('../db/utils');
const {deleteActiveRebalanceSync} = require('../db/utils');
const {recordLiquidity} = require('../db/utils');
const {listPeersMapSync} = require('../lnd-api/utils');
const {getNodeFeeSync} = require('../lnd-api/utils');
const {rebalanceSync} = require('../bos/rebalance');

const arrAvg = arr => arr.reduce((a,b) => a + b, 0) / arr.length;
const stringify = obj => JSON.stringify(obj, null, 2);
const errcode = err => err.err && err.err[2] && err.err[2].err && err.err[2].err[1];


// keep track of nodes to report stats
const REPS = 2;
const PPM_PET_HOP = 350;
const MIN_PPMS_TRIES = 4; // min ppm occurances before attempting to exclude a node.
                          // the greater the number, the more chances nodes get
                          // to prove they are not expensive before being excluded

module.exports = ({from, to, amount, ppm = config.rebalancer.maxPpm || constants.rebalancer.maxPpm, mins, avoidArr = config.avoid || [], type}) => {
  if (!from || !to || !amount) {
    throw new Error('from, to and amount are mandatory arguments');
  }

  logger.log('rebalancer is starting up');

  var peerMap = listPeersMapSync(lndClient);

  const OUT = from;
  const IN = to;
  const AMOUNT = amount;

  var tagsMap = {};
  Object.keys(tags).forEach(t => tagsMap[tags[t]] = t);

  // find ids
  var outId = findId(OUT);
  var inId = findId(IN);

  if (!outId) {
    throw new Error('couldnt find pub id for ' + OUT);
  }
  if (!inId) {
    throw new Error('couldnt find pub id for ' + IN);
  }

  // find names
  var outName = peerMap[outId].name;
  var inName = peerMap[inId].name;

  // check that the exit node's fee isn't higher than ppm
  let fee = getNodeFeeSync(lndClient, inId);
  if (fee) {
    let sum = fee.base/1000 + fee.rate;
    if (sum > ppm) {
      return logger.error(`exit node's fee, base:${fee.base} rate:${fee.rate}, exceeds supplied ppm of ${ppm}`);
    } else if (100 * (ppm - sum) / ppm < 25) {   // enough fee buffer??
      logger.log(`exit node's fee is within 25% of supplied ppm, the buffer may not be sufficient to rebalance`);
    }
  } else {
    throw new Error(`couldnt find fee for ${inId}`);
  }

  var maxFee = Math.round(AMOUNT * ppm / 1000000);
  var ppm_per_hop = Math.min(PPM_PET_HOP, Math.round(.75 * ppm));
  var avoidNodes = {};

  var nodeStats = {};
  var routes = [];
  var lastMessage;
  var lastError;
  var amountRebalanced = 0;
  var feesSpent = 0;
  var skippedHops = {};

  const maxRuntime = mins || config.rebalancer.maxTime || constants.rebalancer.maxTime;

  // it takes time for the rebalancer to properly explore routes. the less time
  // its given, the less is the opportunity to find the cheapest route.
  // the rebalancer needs to be more aggressive when it comes
  // to skipping routes when it is given less than N minutes. the threshold
  // is a guess atm, but one hour seems reasonable.
  const aggresiveMode = maxRuntime < 60;

  // construct avoid string based on argument
  var avoid = "";
  var nodeInfo = {};
  if (avoidArr && avoidArr.length > 0) {
    logger.log('excluding the following nodes based on config:');
    avoidArr.forEach(n => {
      logger.log('  ' + n);
      avoidNodes[n] = true;
    })
  }
  // construct avoid string based on db avoid records
  // the depth of avoid lookup is a guess atm, but having
  // it be at least an hour makes sense. the longer rebalance
  // runs, the more work it does to determine expensive node,
  // the greater the depth of history.
  let avoidDepth = Math.max(60, 5 * maxRuntime);
  let dbAvoid = listRebalanceAvoidSync(outId, inId, ppm, avoidDepth);
  if (dbAvoid && dbAvoid.length > 0) {
    logger.log('excluding the following nodes based on past rebalances:');
    dbAvoid.forEach(n => {
      logger.log('  ' + n);
      avoidNodes[n] = true;
    })
  }

  let info = getNodesInfoSync(lndClient, Object.keys(avoidNodes));
  if (info) {
    info.forEach(n => {
      if (n) nodeInfo[n.node.pub_key] = n;  // can be null if a node mentioned in the config doesn't exist
    })
  }
  let epoch = Math.floor(+new Date() / 1000);
  const keys = Object.keys(avoidNodes);
  if (keys && keys.length > 0) {
    logger.log('more info on the excluded nodes:');
    keys.forEach(node => {
      if (nodeInfo[node]) {
        logger.log('  ' + node + ', ' + nodeInfo[node].node.alias + ', channels: '  + nodeInfo[node].num_channels + ', last updated', Math.round((epoch - nodeInfo[node].node.last_update)/3600), 'hours ago');
        avoid += ' --avoid ' + node;
      }
    })
  }

  // store min ppm of a failed route due to high fee;
  // present it as a stat in jet rebalance-history
  let minFailedPpm = Number.MAX_SAFE_INTEGER;

  logger.log('----------------------------------------')
  logger.log(`from: ${outName}, ${outId}`);
  logger.log(`to: ${inName}, ${inId}`);
  logger.log('amount:', numberWithCommas(AMOUNT));
  logger.log('max ppm:', ppm);
  logger.log('max fee:', maxFee);
  if (type) logger.log('type:', type);
  logger.log('ppm per hop:', ppm_per_hop);
  logger.log('time left:', maxRuntime, 'mins');
  logger.debug('aggressive mode:', (aggresiveMode) ? 'on' : 'off');

  // record for jet monitor
  const rebalanceId = recordActiveRebalanceSync({from: outId, to: inId, amount: AMOUNT, ppm, mins: maxRuntime});
  if (rebalanceId) logger.debug('rebalance id:', rebalanceId);
  else logger.error('rebalance db record id is undefined');

  const startTime = Date.now();

  // run the loop for bos rebalance
  try {
    for (let rep = 0; rep < REPS; ) {
      const iterationStart = Date.now();
      let timeRunning = Math.round((Date.now() - startTime) / 1000 / 60);
      let timeLeft = maxRuntime - timeRunning;
      // stop at 0 as well: bos reads mins: 0 as no time limit at all
      if (timeLeft <= 0) {
        logger.log('ran out of time');
        lastMessage = 'ran out of time';
        break;
      }

      let remainingAmount = AMOUNT - amountRebalanced;
      maxFee = Math.round(remainingAmount * ppm / 1000000);

      logger.log('-------------------------------------------');
      if (amountRebalanced > 0) logger.log('targeted amount:', numberWithCommas(AMOUNT));
      logger.log('remaining amount:', numberWithCommas(remainingAmount));
      logger.log('time left:', timeLeft, 'mins');

      // call bos rebalance; async logger will be notified about route evals and log messages
      let lastRoute;  // last route that was evaluated
      let lastAmount; // last amount that was evaluated
      const rebalanceLogger = {
        eval: (route) => {
          logger.log('probing route,', route.length, 'hops');
          logger.debug(stringify(route));
          lastRoute = route;

          // record the node for sats
          route.forEach(node => {
            if (nodeStats[node.id]) {
              let n = nodeStats[node.id];
              n.ppms.push(node.ppm);
            } else {
              nodeStats[node.id] = {
                name: node.name,
                id: node.id,
                ppms: [node.ppm]
              }
            }

            if (node.ppm > ppm_per_hop && canAvoidNode(node.id)) {
              logger.debug('identified expensive node:', stringify(node));
            }
          })
        },
        amount: (amount) => {
          logger.log('evaluating amount:', amount);
          lastAmount = amount;
        },
        debug: (msg) => {
          logger.debug('bos rebalance debug:', stringify(msg));
        },
        info: (msg) => {
          logger.debug('bos rebalance info:', stringify(msg));
        },
        warn: (msg) => {
          logger.warn('bos rebalance warn:', stringify(msg));
        },
        error: (msg) => {
          const code = errcode(msg);
          if (code === 'TemporaryChannelFailure') logger.debug('(TemporaryChannelFailure) insufficient liquidity on one of the route hops, skip');
          else logger.error('bos rebalance error:', stringify(msg));
        }
      }

      // call bos rebalance in sync mode
      let rbSuccess, rbError;
      try {
        let rbSync = rebalanceSync({logger: rebalanceLogger, from: outId, to: inId, amount: remainingAmount.toString(), maxFeeRate: ppm, maxFee, mins: timeLeft, avoid: Object.keys(avoidNodes)});
        rbSuccess = rbSync.result;
        rbError = rbSync.error;
      } catch(err) {
        logger.error('error calling bos rebalance:', err.message);
        // force to exit the loop, otherwise may get into infinite loop
        rep = REPS;
        continue;
      }

      if (rbError) {
        let rebalanceFeeTooHigh = ['RebalanceTotalFeeTooHigh', 'RebalanceFeeRateTooHigh'].includes(rbError.error);
        let failedToFindPath = rbError.error === 'FailedToFindPathBetweenPeers';
        let lowRebalanceAmount = rbError.error === 'LowRebalanceAmount';
        let failedToFindPeer = rbError.error === 'FailedToFindPeerAliasMatch';
        let noSufficientBalance = rbError.error === 'NoOutboundPeerWithSufficientBalance';
        let probeTimeout = rbError.error === 'ProbeTimeout';
        let failedToParseAmount = rbError.error === 'FailedToParseSpecifiedAmount';
        let unexpectedError = rbError.error === 'UnexpectedErrInGetRouteToDestination';

        if (rebalanceFeeTooHigh) {
          // find nodes that exceed the per hop ppm in the last
          // segment of the output
          lastError = 'rebalanceFeeTooHigh';
          logger.log('found a prospective route, but the fee is too high');
          //logger.log('evaluating output:', stdout);
          //let index = stdout.lastIndexOf('evaluating:');
          if (lastRoute) {
            let nodes = lastRoute;
            if (nodes) {
              // record nodes in the db for further analysis
              nodes.forEach(n => {
                recordLiquidity({node: n.id, sats: lastAmount, ppm: n.ppm});
              })

              // find a node with max ppm that's not already on the avoid list
              // its enough to select one node to unblock the route
              let max;
              let maxIndex;
              let ppmsum = 0;
              let count = 0;
              nodes.forEach(n => {
                ppmsum += n.ppm;
                if (canAvoidNode(n.id) && (!max || n.ppm > max.ppm)) {
                  max = n;
                  maxIndex = count;
                }
                count++;
              })
              logger.debug('the route has', nodes.length, 'nodes:', stringify(nodes));
              logger.log('the route has a [cumulative] ppm of', ppmsum, 'vs', ppm, 'targeted');
              minFailedPpm = Math.min(minFailedPpm, ppmsum);
              if (max) {
                logger.debug('identified expensive node to exclude:', stringify(max));
                if (max.ppm > ppm_per_hop) {
                  let entry = nodeStats[max.id];
                  logger.debug('identified corresponding nodeStats entry:', nodeToString(entry));

                  // in addressive more just skip the node, as opposed to
                  // giving the node more chances to prove that it's not
                  // expensive
                  if (aggresiveMode) {
                    logger.debug('aggressive mode: on');
                    logger.log('excluding node:', max.id);
                    avoidNodes[max.id] = true;
                    avoid += ' --avoid ' + max.id;
                    // record in the db
                    recordRebalanceAvoid(outId, inId, ppm, max.id);
                  } else {
                    // see if the node is a repeat offender
                    // basically give the node a few chances to show that it's
                    // not an expensive node before excluding it
                    if (entry.ppms.length >= MIN_PPMS_TRIES && arrAvg(entry.ppms) > ppm_per_hop) {
                      logger.debug('the node was part of', entry.ppms.length, 'routes with an average ppm of', Math.round(arrAvg(entry.ppms)));
                      logger.log('excluding node:', max.id);
                      avoidNodes[max.id] = true;
                      avoid += ' --avoid ' + max.id;
                      // record in the db
                      recordRebalanceAvoid(outId, inId, ppm, max.id);
                    } else {
                      // give the node a few more tries, but don't exclude it
                      if (isSkippedHop(max.id, nodes[maxIndex + 1].id)) {
                        lastMessage = 'hop already skipped';
                        logger.debug('hop from', max.name, 'to', nodes[maxIndex + 1].name, 'already skipped, exiting');
                        rep = REPS;
                      } else {
                        lastMessage = 'skipping the hop';
                        skipHop(max.id, nodes[maxIndex + 1].id);
                        logger.debug('skipping the hop from', max.name, 'to', nodes[maxIndex + 1].name);
                        avoid += ' --avoid "FEE_RATE>' + computeFeeRate(max.ppm) + '/' + nodes[maxIndex + 1].id + '"';
                      }
                    }
                  }
                } else {  // max.ppm <= ppm_per_hop
                  // ppm is not greater than max; don't exclude the node
                  // but rather skip the hop
                  if (isSkippedHop(max.id, nodes[maxIndex + 1].id)) {
                    lastMessage = 'hop already skipped';
                    logger.debug('hop from', max.name, 'to', nodes[maxIndex + 1].name, 'already skipped, exiting');
                    rep = REPS;
                  } else {
                    lastMessage = 'ppm is not greater than max, skipping the hop';
                    skipHop(max.id, nodes[maxIndex + 1].id);
                    logger.debug(lastMessage, 'from', max.name, 'to', nodes[maxIndex + 1].name);
                    avoid += ' --avoid "FEE_RATE>' + computeFeeRate(max.ppm) + '/' + nodes[maxIndex + 1].id + '"';
                  }
                }
              } else {  // !max
                lastMessage = 'couldnt exclude any nodes, likely already on the avoid list';
                logger.log(lastMessage + ', retrying');
                rep++;
              }
            } else {  // !nodes
              lastMessage = 'couldnt exclude any nodes';
              logger.log(lastMessage + ', retrying');
              rep++;
            }
          } else {
            lastMessage = 'couldnt locate the segment of the output to analyze';
            logger.log(lastMessage + ', retrying');
            rep++;
          }
        } else if (failedToFindPath) {
          // didn't find a route; last ditch effort - exclude all expensive nodes
          // that have not yet been excluded and retry
          lastError = 'failedToFindPath';
          lastMessage = 'failed to find a route';
          logger.log('-------------------------------------------');
          logger.log(lastMessage);
          logger.debug('exclude all expensive nodes and retry');
          let count = 0;
          Object.keys(nodeStats).forEach(id => {
            if (canAvoidNode(id) && arrAvg(nodeStats[id].ppms) > ppm_per_hop) {
              logger.log('excluding node:', nodeStats[id].name);
              avoidNodes[id] = true;
              avoid += ' --avoid ' + id;
              count++;
            }
          })
          if (count > 0) {
            logger.debug('excluded', count, 'nodes');
          } else {
            lastMessage += ', didnt find any nodes to exclude';
            logger.log('didnt find any nodes to exclude, retrying');
            rep++;
          }
        } else if (lowRebalanceAmount) {
          lastError = 'lowRebalanceAmount';
          lastMessage = 'low rebalance amount';
          logger.log('-------------------------------------------');
          logger.log(lastMessage + ', exiting');
          rep = REPS; // force to exit the loop
        } else if (failedToFindPeer) {
          lastError = 'failedToFindPeer';
          lastMessage = 'failed to find peer'
          logger.log(lastMessage + ', exiting');
          rep = REPS; // force to exit the loop
        } else if (noSufficientBalance) {
          lastError = 'noSufficientBalance';
          lastMessage = 'insufficient local balance';
          logger.log(lastMessage + ', exiting');
          rep = REPS;
        } else if (probeTimeout) {
          lastError = 'probeTimeout';
          lastMessage = 'ran out of time';
          logger.log(lastMessage + ', exiting');
          rep = REPS;
        } else if (failedToParseAmount) {
          lastError = 'FailedToParseSpecifiedAmount';
          lastMessage = 'failed to parse amount';
          logger.log(lastMessage + ', exiting');
          rep = REPS;
        } else if (unexpectedError) {
          lastError = 'unexpectedError';
          lastMessage = 'unexpected error';
          logger.log('-------------------------------------------');
          logger.log(lastMessage, JSON.stringify(rbError, null, 2), ' exiting');
          rep = REPS;
        } else {
          lastError = 'unidentifiedError';
          lastMessage = 'unidentified error';
          logger.log('-------------------------------------------');
          logger.log(lastMessage, JSON.stringify(rbError, null, 2), ' exiting');
          rep = REPS;
        }
      } else {  // !stderr
        logger.log('-------------------------------------------');
        lastMessage = 'successful rebalance';
        // determine amount rebalanced
        let amount = rbSuccess.amount;
        let fees = rbSuccess.fees;
        if (amount > 0) {
          logger.log('amount rebalanced:', numberWithCommas(amount));
          amountRebalanced += amount;

          // record result in the db for further optimation
          recordRebalance(iterationStart, outId, inId, AMOUNT, amount, Math.round(1000000 * fees / amount), type);

          // record nodes on the route for future analysis
          if (lastRoute) {  // shouldn't be empty but just in case
            lastRoute.forEach(n => {
              recordLiquidity({node: n.id, sats: amount, ppm: n.ppm});
            })
          }

          logger.log('total amount rebalanced:', numberWithCommas(amountRebalanced));
          if (fees > 0) {
            logger.log('fees spent:', fees);
            feesSpent += fees;
            logger.log('total fees spent:', feesSpent);
            logger.log('ppm:', Math.round(1000000 * feesSpent / amountRebalanced));
          } else {
            lastMessage = 'couldnt parse fees';
            logger.log(lastMessage + ', retrying');
          }
          if (amountRebalanced > AMOUNT) {
            logger.log('amount rebalanced exceeds targeted, exiting the loop');
            rep = REPS;
          } else if (AMOUNT - amountRebalanced < 50000) {
            logger.log('less than 50k to rebalance, exiting the loop');
            rep = REPS;
          }
        } else {
          lastMessage = 'successful rebalance, but couldnt extract amount rebalanced';
          logger.log(lastMessage + ', exiting');
          rep = REPS; // force to exit the loop
        }
      } // if stderr

      // helper function
      function canAvoidNode(id) {
        return !avoidNodes[id] && id !== outId && id !== inId && id !== OUT && id !== IN && id !== tags[OUT] && id !== tags[IN];
      }
    } // for
  } catch(err) {
    logger.error('error running rebalance loop:', err);
  }

  // record rebalance failure, success has already been recorded
  if (amountRebalanced <= 0 && ['rebalanceFeeTooHigh', 'failedToFindPath', 'unexpectedError', 'unidentifiedError'].indexOf(lastError) >= 0) {
    if (minFailedPpm < Number.MAX_SAFE_INTEGER) recordRebalanceFailure(startTime, outId, inId, AMOUNT, lastError, ppm, minFailedPpm, type);
    else recordRebalanceFailure(startTime, outId, inId, AMOUNT, lastError, ppm, 0, type);
  }

  printStats(lndClient, nodeStats, nodeInfo);

  if (rebalanceId) {
    logger.debug('deleting rebalance record with id:', rebalanceId);
    deleteActiveRebalanceSync(rebalanceId);
  } else {
    logger.warn('can not delete rebalance record, id does not exist');
  }

  // each jet rebalance instance runs in a seraparate process; explicitly exit
  // so that processes don't linger; this isn't ideal, but it ensures
  // that node operators don't have to manually kill processs that
  // are stuck. its unclear why some processes are getting stuck.
  // this issue will become moot once jet moves to a single-process
  // architecture. note that explicit process exit should not result in
  // adverse side effects, e.g no pending db writes that may result
  // in a corrupted db once interrupted
  // https://github.com/itsneski/lightning-jet/issues/55
  process.exit();

  // str can either be a tag, a portion of node's alias, or node's pub id
  function findId(str) {
    if (tags[str]) return tags[str];
    if (peerMap[str]) return str;
    // see if str is part of an alias
    let id;
    Object.values(peerMap).forEach(p => {
      if (p.name.toLowerCase().indexOf(str.toLowerCase()) >= 0) {
        if (id) throw new Error('more than one peer associated with ' + str + '; narrow your selection');
        id = p.id;
      }
    })
    return id;
  }

  // format for printing
  function printStats() {
    getNodesInfoSync(lndClient, Object.keys(nodeStats)).forEach(n => {
      if (n) nodeInfo[n.node.pub_key] = n;
    })
    epoch = Math.floor(+new Date() / 1000);
    let stats = Object.values(nodeStats);
    stats.forEach(n => { 
      n.avg_ppm = Math.round(arrAvg(n.ppms));
      n.max_ppm = Math.max.apply(Math, n.ppms);
      n.channels = nodeInfo[n.id].num_channels;
      n.updated_h_ago = Math.round((epoch - nodeInfo[n.id].node.last_update)/3600);
    })
    stats.forEach(n => n.ppms = n.ppms.join(','));
    stats.sort(function(a, b) {
      return b.avg_ppm - a.avg_ppm;
    })

    // splint into two groups based on ppm_per_hop
    let sortedMax = stats.filter(n => n.avg_ppm > ppm_per_hop);
    let lowFeeSorted = stats.filter(n => n.avg_ppm <= ppm_per_hop);

    let routesFormatted = [];
    routes.forEach(route => {
      let r = {
        amount: numberWithCommas(route[0].amount),
        ppm: Math.round(1000000 * route[0].fees / route[0].amount),
        nodes: []
      }
      route.slice(1).forEach(n => {
        r.nodes.push(n.name + ', ' + n.ppm + ', ' + n.id);
      })
      routesFormatted.push(r);
    })

    logger.log('-------------------------------------------');
    logger.log('finished rebalance from', OUT, 'to', IN);
    logger.debug('last message:', lastMessage);
    logger.log('amount targeted:', numberWithCommas(AMOUNT));
    logger.log('amount rebalanced:', numberWithCommas(amountRebalanced));
    if (feesSpent > 0) {
      logger.log('fees spent:', feesSpent);
      logger.log('ppm: ', Math.round(1000000 * feesSpent / amountRebalanced));
    }
    if (routesFormatted.length > 0) logger.log('routes:', stringify(routesFormatted));
    logger.debug('nodes that exceeded per hop ppm:', stringify(sortedMax));
    logger.debug('low fee nodes:', stringify(lowFeeSorted));
    logger.log('-------------------------------------------');
  }

  function computeFeeRate(ppm) {
    // based on Alex B: route ppm includes base fee and is randomized.
    // ensure that there is enough of a delta to skip the route for
    // --avoid "FEE_RATE>". is 95% of node's ppm sufficient? e.g.
    // for 250 ppm, "FEE_RATE>238", with 12 sats threshold.
    return Math.min(ppm_per_hop, Math.round(ppm * .95));
  }

  function skipHop(a, b) {
    if (!skippedHops[a]) skippedHops[a] = [];
    skippedHops[a].push(b);
  }

  function isSkippedHop(a, b) {
    return skippedHops[a] && skippedHops[a].includes(b);
  }
}

function numberWithCommas(x) {
  return x.toString().replace(/\B(?<!\.\d*)(?=(\d{3})+(?!\d))/g, ",");
}

function nodeToString(n) {
  let c = {
    name: n.name,
    id: n.id,
    max_ppm: Math.max.apply(Math, n.ppms),
    avg_ppm: Math.round(arrAvg(n.ppms)),
    ppms: n.ppms.join(',')
  }
  return stringify(c);
}
