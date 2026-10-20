const logger = require('./logger');
const constants = require('./constants');
const config = require('./config');
const importLazy = require('import-lazy')(require);
const lndClient = importLazy('./connect');
const {listFeesSync} = require('../lnd-api/utils');
const {feeHistorySync} = require('../db/utils');
const {classifyPeersSync} = require('./utils');

module.exports = {
  rebalanceStatus() {
    const pref = 'rebalanceStatus:';
    const analyzeFees = module.exports.analyzeFees;
    let classified = classifyPeersSync(lndClient);
    let chans = [];
    classified.outbound.forEach(c => chans.push({chan: c.id, peer: c.peer}));
    classified.balanced.forEach(c => chans.push({chan: c.id, peer: c.peer}));
    let fees = listFeesSync(lndClient, chans);
    let feeMap = {};
    fees.forEach(f => feeMap[f.id] = f);

    const action = constants.feeAnalysis.action;

    let outbound = doIt(classified.outbound);
    let balanced = doIt(classified.balanced);
    return { outbound: outbound, balanced: balanced };

    function doIt(peerList) {
      let arr = [];
      peerList.forEach(c => {
        if (!feeMap[c.peer]) {
          return logger.debug(pref, 'fee map for', c.name, 'does not exist, skip');
        }
        let list = analyzeFees(c.name, c.peer, feeMap[c.peer].local, feeMap[c.peer].remote);
        let entry = {
          peer: c.name,
          status: (list[0].action === action.pause) ? 'paused' : 'active'
        }
        entry['local'] = feeMap[c.peer].local.rate;
        entry['remote'] = feeMap[c.peer].remote.rate;
        if (list[0].maxPpm) entry['max ppm'] = list[0].maxPpm;
        if (list[0].range) entry.range = list[0].range;
        if (list[0].summary) entry.summary = list[0].summary;
        arr.push(entry);
      })
      arr.sort(function(a, b) {
        let a_max = a['max ppm'] || 0;
        let b_max = b['max ppm'] || 0;
        return b_max - a_max;
      })
      return arr;
    }
  },
  printFeeAnalysis(peerName, peerId, localFee, remoteFee, profit = 0) {
    let msgs = module.exports.analyzeFees(peerName, peerId, localFee, remoteFee, profit);
    const action = constants.feeAnalysis.action;
    let status = [];
    msgs.forEach(m => {
      let color;
      if (m.importance === constants.feeAnalysis.urgent) color = constants.colorRed;
      if (m.importance === constants.feeAnalysis.warning) color = constants.colorYellow;
      if (m.importance === constants.feeAnalysis.success) color = constants.colorGreen;
      if (color) console.log(color, m.message);
      else console.log(m.message);

      // rebalancing status
      if (m.action === action.pause) status.push('action: pause');
      if (m.maxPpm) status.push('current max ppm: ' + m.maxPpm);
      if (m.suggestedPpm) status.push('suggested max ppm: ' + m.suggestedPpm);
      if (m.summary) status.push('summary: ' + m.summary);
    })
    if (global.testModeOn && status.length > 0) console.log('(' + status.join(', ') + ')');
    return msgs.length;
  },
  // analyzes peer fees and returns an array of messages with importance
  // importance grades: normal, warning, urgent
  // profit - profit margin to evaluate fees against; default - 0, means
  // that there arent any paricular profit requirements. the rebalancer
  // will attempt to rebalance as long as its not at a loss
  analyzeFees(peerName, peerId, localFee, remoteFee, profit = 0) {
    if (profit < 0 || profit > 100) throw new Error('profit has to be between 0 and 100'); 

    let local = Math.round(localFee.base/1000 + localFee.rate);
    let remote = Math.round(remoteFee.base/1000 + remoteFee.rate);

    const normal = constants.feeAnalysis.normal;
    const warning = constants.feeAnalysis.warning;
    const urgent = constants.feeAnalysis.urgent;
    const success = constants.feeAnalysis.success;
    const action = constants.feeAnalysis.action;

    const maxPpm = global.testMaxPpm || config.rebalancer.maxAutoPpm || constants.rebalancer.maxAutoPpm;
    const enforceMaxPpm = (global.testEnforceMaxPpm === undefined) ? config.rebalancer.enforceMaxPpm : global.testEnforceMaxPpm;
    const minBuffer = constants.rebalancer.minBuffer;
    const buffer = config.rebalancer.buffer || constants.rebalancer.buffer;
    const enforceProfitability = (global.testEnforceProfitability === undefined) ? config.rebalancer.enforceProfitability : global.testEnforceProfitability;

    let optimalMaxPpm;
    let array = [];

    // get recent fee changes for the peer
    const feeHistoryDepth = constants.feeAnalysis.historyDepth;
    let feeHistory = feeHistorySync({node:peerId, mins:feeHistoryDepth * 60});
    let feeStats;
    if (feeHistory && feeHistory.length > 0) {
      let min;
      let max;
      let count = 0;
      feeHistory.forEach(h => {
        if (h.ppm > 0) {
          min = (min) ? Math.min(min, h.ppm) : h.ppm;
          max = (max) ? Math.max(max, h.ppm) : h.ppm;
          count++;
        }
      })
      if (count > 0) {
        feeStats = { count: count, min: min, max: max };
      }
    }

    // formulate intro
    let status = { importance: normal, message: 'evaluating [outbound] ' + peerName };
    array.push(status);

    let intro = 'current fees: local { base: ' + localFee.base + ', ppm: ' + localFee.rate + ' } remote { base: ' + remoteFee.base + ', ppm: ' + remoteFee.rate + ' }. max ppm: ' + maxPpm + ' (';
    intro += (enforceMaxPpm) ? 'enforced)' : 'not enforced)';
    intro += ', rebalance buffer is ' + buffer + ' ppm';
    if (enforceProfitability) intro += ', profitability is enforced';
    if (profit) intro += ', profit margin: ' + profit + '%';
    if (feeStats) intro += '\nthe peer changed ppm ' + feeStats.count + ' time(s) over the past ' + feeHistoryDepth + ' hours, min: ' + feeStats.min + ', max: ' + feeStats.max;
    addMessage(normal, intro);

    // check if the node operator intends to accumulate local liquidity,
    // with 10x difference being an indication
    let x = Math.floor(local/maxPpm);
    if (x > 10) {
      addMessage(warning, 'local ppm exceeds max ppm by more than ' + x + 'x. assuming node operator intends to accumulate sats');
      addMessage(urgent, 'pausing rebalancing for this peer');
      addMessage(normal, 'suggestion: consider revisiting local ppm to resume rebalancing');
      status.summary = 'local ppm is too high, revisit';
      status.action = action.pause;
      return array;
    }

    let optimal = maxPpm;
    if (local > maxPpm) {
      if (enforceMaxPpm) {
        addMessage(normal, 'local ppm exceeds the max ppm, setting optimal max ppm to ' + maxPpm);
        optimal = maxPpm;
      } else {
        // extra warning about local/maxPpm ratio
        if (x >= 2) {
          // not critical?
          addMessage(warning, 'local ppm exceeds max ppm by more than ' + x + 'x');
          addMessage(normal, 'setting optimal max ppm to ' + local);
        } else {
          addMessage(normal, 'setting optimal max ppm to ' + local);
        }
        optimal = local;
      }
    } else {
      addMessage(normal, 'setting optimal max ppm to ' + local);
      optimal = local;  // local is below the max, so assume it as the max
    }

    // check for anomalies
    let xx = Math.ceil(remote / maxPpm);
    if (xx >= 10) {
      addMessage(warning, 'remote fee exceeds the max ppm by more than ' + xx + 'x');
      addMessage(normal, 'keep on monitoring peer\'s fees');
      status.summary = 'remote fee exceeds max ppm by than ' + xx + 'x';
      status.action = action.pause;
      return array;
    }

    // profitability is not enforced
    let override = !enforceProfitability && optimal <= remote && maxPpm > optimal;
    if (override) {
      // just need to ensure there is enough of a buffer to rebalance, no
      // need to go overboard
      let val = Math.min(remote + buffer, maxPpm);
      addMessage(warning, 'overriding optimal max ppm with ' + val);
      optimal = val;
    }

    // can rebalance be profitable
    if (optimal <= remote) {
      if (optimal < remote) addMessage(warning, 'remote fee exceeds the optimal max ppm');
      else addMessage(warning, 'remote ppm equals optimal max ppm');

      addMessage(urgent, 'pausing rebalancing for this peer');
      // see if it makes to make ppm reco
      if (feeStats && feeStats.count >= 2) {
        if (feeStats.min < remote) {
          // the peer has been flip flopping between fees, with the min being below
          // remote, meaning there is still chance that the fee will be reversed
          addMessage(normal, 'the peer has been changing fees fairly often, with the min of ' + feeStats.min + ' that is less than remote fee');
          addMessage(normal, 'suggestion: keep on monitoring peer\'s fees, there is a chance the peer will reduce fees');
        } else {
          // is there a hope for the peer to drop fees?
          addMessage(normal, 'suggestion: keep on monitoring peer\'s fees');
        }
        if (optimal < remote) status.summary = 'remote ppm exceeds local. keep on monitoring peer\'s fees';
        else status.summary = 'remote ppm equals local. keep on monitoring peer\'s fees';
      } else {  // no fee history
        let suggested = remote + buffer;
        if (profit) suggested *= (1 + profit/100);
        let range = '[' + (remote + minBuffer) + ' - ' + suggested + ']';
        status.range = range;
        if (enforceMaxPpm && remote > maxPpm) {
          let msg = 'suggested local ppm and / or max ppm range: ' + range;
          addMessage(normal, msg);
          status.summary = 'remote ppm exceeds max ppm. revisit max ppm based on suggested range';
        } else {
          let msg = 'suggested local ppm range: ' + range;
          addMessage(normal, msg);
          status.summary = 'remote ppm exceeds local. revisit local ppm based on suggested range';
        }
        status.suggestedPpm = suggested;
      }
      status.action = action.pause;
      return array; // serious enough to exit
    }

    // is there a sufficient buffer to rebalance profitably
    if (profit) {
      let minProfitable = Math.ceil((remote + buffer) * (1 + profit/100));
      let profitAdjusted = Math.round(optimal * (1 - profit / 100));
      addMessage(normal, 'profit-adjusted optimal max ppm is ' + profitAdjusted);
      addMessage(normal, 'minimum max ppm needed to account for remote fee & buffer is ' + minProfitable);
      let range = '[' + (remote + minBuffer) + ' - ' + minProfitable + ']';
      status.range = range;
      if (profitAdjusted < minProfitable) {
        addMessage(warning, 'insufficient buffer for rebalancer to meet profitability margin of ' + profit + '%');

        let override = !enforceProfitability && maxPpm > optimal;
        if (override) {
          // just need to make there is enough of a buffer to rebalance
          let val = Math.min(minProfitable, maxPpm);
          addMessage(warning, 'overriding optimal max ppm with ' + val);
          optimal = val;
        }

        // check if the peer has been frequently changing its fee
        if (feeStats && feeStats.count >= 2) {
          status.summary = 'peer changed fees ' + feeStats.count + ' times over past ' + feeHistoryDepth + ' hours with min of ' + feeStats.min + ' sats';
        } else {
          status.summary = (override) ? 'override, ' : '';
          if (enforceMaxPpm && minProfitable > maxPpm) {
            addMessage(normal, 'suggested local ppm and/or max ppm range: ' + range);
            status.summary += 'increase local ppm and/or max ppm to meet profitability. suggested range: ' + range;
          } else {
            addMessage(normal, 'suggested local ppm range: ' + range);
            status.summary += 'increase local ppm based on suggested range';
          }
          status.suggestedPpm = minProfitable;
        }

        status.maxPpm = optimal;  // insufficient buffer to account for profit %
                                  // revert to the current optimal max ppm
      } else {
        addMessage(normal, 'optimal max ppm meets profitability margin of ' + profit + '%');
        if (override) status.summary = 'max ppm override';
        status.maxPpm = profitAdjusted;
      }
    } else {  // profit requirements not specified
      if (optimal < remote + buffer) {
        addMessage(normal, 'insufficient buffer between optimal max and remote ppm, rebalances have less of a chance to go through');

        let override = !enforceProfitability && maxPpm > optimal;
        if (override) {
          // just need to make there is enough of a buffer to rebalance
          let val = Math.min(remote + buffer, maxPpm);
          addMessage(warning, 'overriding optimal max ppm with ' + val);
          optimal = val;
        }

        // check if the peer has been frequently changing its fee
        if (feeStats && feeStats.count >= 2) {
          status.summary = 'peer changed fees ' + feeStats.count + ' times over past ' + feeHistoryDepth + ' hours with min of ' + feeStats.min + ' sats';
        } else {
          let range = '[' + (remote + minBuffer) + ' - ' + (remote + buffer) + ']';
          status.range = range;
          status.summary = (override) ? 'override, ' : '';
          if (enforceMaxPpm && remote + buffer > maxPpm) {
            let msg = 'suggested local ppm and / or max ppm range: ' + range;
            addMessage(warning, msg);
            status.summary += 'consider increasing local ppm and/or max ppm within the suggested range'
          } else {
            let msg = 'suggested local ppm range: ' + range;
            addMessage(warning, msg);
            status.summary += 'consider increasing local ppm within the suggested range'
          }
          status.suggestedPpm = remote + buffer;
        }
      } else {
        addMessage(normal, 'sufficient buffer between remote ppm and optimal max ppm. things are looking good');
        if (override) status.summary = 'max ppm override';
      }
      if (status.maxPpm === undefined) status.maxPpm = optimal;
    }
    return array;

    function addMessage(importance, message) {
      array.push({importance:importance, message:message})
    }
  }
}
