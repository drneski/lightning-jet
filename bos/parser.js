
const {removeEmojis} = require('../lnd-api/utils');

module.exports = {
  parseNodes(str) {
    if (!str['evaluating']) return;
    let nodes = [];
    str['evaluating'].forEach(line => {
      let node = module.exports.parseNode(line);
      if (node) nodes.push(node);
    })
    return nodes;
  },

  parseNode(str) {
    let index = str.indexOf(". Fee rate");
    if (index < 0) return;

    let part1 = str.substring(0, index);
    let part2 = str.substring(index + 1);
    let parts = part1.split(/(\s+)/).filter(e => e.trim().length > 0);

    let id = parts[parts.length - 1];
    let name = normalizeString(part1.substring(0, part1.indexOf(id)));
    let ppm = parseInt(part2.substring(part2.indexOf('(') + 1, part2.indexOf(')')))

    return {id, name, ppm}
  },

  parseError(err) {
    //console.log('parseError:', err);
    if (!err) return;
    let code = err[0];
    if (!code) return; // something is off
    let error = err[1];
    let ret = { code, error };
    if (error === 'RebalanceFeeRateTooHigh') ret.maxFeeRate = parseInt(err[2].needed_max_fee_rate);
    else if (error === 'RebalanceTotalFeeTooHigh') ret.maxFee = parseInt(err[2].needed_max_fee);
    ret.original = err;
    //console.log('parseError:', ret);
    return ret;
  },

  parseResult(res) {
    if (!res) return; // something is off
    if (!res['rebalance']) return; // something is off
    let arr = res['rebalance'];
    let ret = {};
    if (arr.length < 3) { // something is off
      ret.original = res;
      return ret;
    }
    const parseVal = (val) => Math.round(parseFloat(val) * 100000000);
    const parsePpm = (ppm) => {
      const m = (typeof ppm === 'string') && ppm.match(/\((.*?)\)/);
      return m ? parseInt(m[1]) : 0;
    }
    ret.amount = parseVal(arr[2]['rebalanced']);
    // bos leaves the fee out when it is under 1 sat: that is a fee of 0, and
    // parsing the missing value would record NaN
    const spent = arr[2]['rebalance_fees_spent'];
    ret.fees = (spent === undefined) ? 0 : parseVal(spent);
    ret.ppm = parsePpm(arr[2]['rebalance_fee_rate']);
    ret.original = res;
    return ret;
  }
}

function normalizeString(str) {
  const skip = /(?:['\x1B]|[[92m][[39m])/g;
  let normalized = removeEmojis(str);
  return normalized.replace(skip, String()).trim();
}
