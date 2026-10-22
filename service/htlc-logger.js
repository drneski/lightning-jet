// logs htlcs into the jet db.  filters htlcs by failed htlcs due to
// insufficient funds.  skips the probes.
//
// logs channel state updates into the jet db.
//
// jet start logger
// jet stop logger
// jet restart logger

const date = require('date-and-time');
const constants = require('../api/constants');
const routerRpc = require('../api/router-rpc');
const lnRpc = require('../api/ln-rpc');
const lndClient = require('../api/connect');
const {recordHtlc} = require('../db/utils');
const {isRunningSync} = require('../api/utils');
const {setPropSync} = require('../db/utils');
const {recordChannelEvent} = require('../db/utils');
const {isLndAlive} = require('../lnd-api/utils');

const loopInterval = 20; // secs

var lastError;
var lndOffline;

const formattedDate = () => date.format(new Date, 'MM/DD hh:mm:ss A');

// only one instance allowed
const fileName = require('path').basename(__filename);
if (isRunningSync(fileName, true)) {
  return console.error(`${fileName} is already running, only one instance is allowed`);
}

// Both subscriptions pass an empty request: protobufjs 7, which 1.7.0's gRPC
// upgrade brought in, refuses to encode a missing one ("object expected"),
// where 1.6's protobufjs 6 sent an empty message.
async function subscribeToHtlcs() {
  console.log(formattedDate() + ' subscribing to htlc events');
  for await (const event of routerRpc.subscribeHtlcEvents({})) {
    let lf = event.link_fail_event;
    // filter events
    if (lf && lf.wire_failure === 'TEMPORARY_CHANNEL_FAILURE' && lf.failure_detail === 'INSUFFICIENT_BALANCE') {
      logHtlc(event);
    }
  }
}

async function subscribeToChannelEvents() {
  console.log(formattedDate() + ' subscribing to channel events');
  for await (const event of lnRpc.subscribeChannelEvents({})) {
    try {
      logChannelUpdate(event);
    } catch(err) {
      console.error('error logging channel event:', err);
    }
  }
}

function logHtlc(event) {
  if (event.incoming_channel_id === '0') {
    return console.log('skipping htlc since the incoming chan id is 0 (due to rebalance as opposed to a forward)')
  }
  console.log('\n' + date.format(new Date, 'MM/DD hh:mm:ss A'));
  console.log('logging htlc:', event);

  try {
    recordHtlc(event);
  } catch(err) {
    console.log('error logging event:', err.message);
  }
}

function logChannelUpdate(event) {
  const pref = 'logChannelUpdate:';
  const getTxid = s => Buffer.from(s).reverse().toString('hex');
  console.log('\n' + date.format(new Date, 'MM/DD hh:mm:ss A'));
  console.log(pref, event);
  let txid, index;
  if (event.type === 'INACTIVE_CHANNEL') {
    txid = getTxid(event.inactive_channel.funding_txid_bytes);
    index = event.inactive_channel.output_index;
  } else if (event.type === 'ACTIVE_CHANNEL') {
    txid = getTxid(event.active_channel.funding_txid_bytes);
    index = event.active_channel.output_index;
  } else {
    return console.warn(pref, 'unprocessed event', event);
  }
  if (!txid) return console.error(pref, 'error: could not identify transaction id');
  
  // record in the db
  recordChannelEvent(event.type, txid, index);
}

function processError(error) {
  const pref = 'processError:';
  console.error(formattedDate(), pref, error.toString());

  // trigger lnd [is alive] check
  lastError = error;
}

function runLoop() {
  const pref = 'runLoop:';
  try {
    runLoopImpl();
  } catch(err) {
    // trigger restart?
    console.error(formattedDate(), pref, err);
    console.error(formattedDate(), pref, 'triggering restart');
    setPropSync(constants.services.logger.errorProp, err.toString());
  }
}

function runLoopImpl() {
  const pref = 'runLoopImpl:';

  // run the loop on first lnd check or when lnd is offline or when there was an error
  if (lndOffline === undefined || lndOffline || lastError) {
    const prev = lndOffline;
    try {
      lndOffline = !isLndAlive(lndClient);
    } catch(err) {
      console.error(formattedDate(), pref, err.toString(), 'assuming lnd is offline');
      lndOffline = true;
    }
    if (lndOffline) {
      if (prev || prev === undefined) console.warn(constants.colorYellow, formattedDate() + ' lnd is offline');
      else console.error(constants.colorRed, formattedDate() + ' lnd went offline');
    } else if (lastError) {
      // lnd is online, but there was an error; trigger restart just in case
      console.warn(constants.colorYellow, formattedDate() + ' error detected ' + lastError.toString() + '; triggering restart');
      setPropSync(constants.services.logger.errorProp, lastError.toString());
    } else if (prev) {
      console.log(constants.colorGreen, formattedDate() + ' lnd is back online');
      init();
    } else if (prev === undefined) {
      console.log(constants.colorGreen, formattedDate() + ' lnd is online');
      init();
    }
    lastError = undefined;  // reset
  } else {
    // skip
  }
}

function init() {
  subscribeToHtlcs().catch(error => {
    processError(error);
  })

  subscribeToChannelEvents().catch(error => {
    processError(error);
  })
}


runLoop();
setInterval(runLoop, loopInterval * 1000);
