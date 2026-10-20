// https://www.npmjs.com/package/node-telegram-bot-api

const config = require('../api/config');
const constants = require('../api/constants');
const logger = require('../api/logger');
const TelegramBot = require('node-telegram-bot-api');
const {getPropSync} = require('../db/utils');
const {getPropAndDateSync} = require('../db/utils');
const {setPropSync} = require('../db/utils');
const {recordFee} = require('../db/utils');
const {fetchTelegramMessageSync} = require('../db/utils');
const {deleteTelegramMessages} = require('../db/utils');
const importLazy = require('import-lazy')(require);
const lndClient = importLazy('../api/connect');
const {listFeesSync} = require('../lnd-api/utils');
const {classifyPeersSync} = require('../api/utils');
const {analyzeFees} = require('../api/analyze-fees');
const serviceUtils = require('./utils');
const util = require('util');
const date = require('date-and-time');

const stringify = obj => JSON.stringify(obj, null, 2);

const pollInterval = constants.services.telegram.pollInterval;
const feeInterval = constants.services.telegram.feeInterval;

const encode = s => Buffer.from(s).toString('base64');
const decode = s => Buffer.from(s, 'base64').toString();
const formatDate = d => date.format(new Date(d), 'MM/DD hh:mm A')

if (global.testModeOn) {
  // for testing
  module.exports.runMonitor = monitorFeesExec;
} else {
  initBot();
  monitorFees();
  setInterval(monitorFees, feeInterval * 1000);
}

function monitorFees() {
  try {
    monitorFeesExec();
  } catch(error) {
    logger.error(error.message);
  }
}

// monitor fee updates
function monitorFeesExec() {
  logger.log('checking fee updates');

  serviceUtils.TelegramBot.recordHeartbeat('fees');
  let fees = listFeesSync(lndClient);
  let prev = getPropAndDateSync('fees');
  setPropSync('fees', encode(JSON.stringify(fees, null, 2)));
  if (!prev) return;

  let outboundMap = {};
  let classified = classifyPeersSync(lndClient);
  if (classified && classified.outbound) {
    classified.outbound.forEach(c => outboundMap[c.peer] = c.name);
  }
  if (classified && classified.balanced) {
    classified.balanced.forEach(c => outboundMap[c.peer] = c.name);
  }

  let prevFees = JSON.parse(decode(prev.val));
  logger.log('identified existing fees recorded on', formatDate(prev.date));
  let prevMap = {};
  prevFees.forEach(f => prevMap[f.chan] = f);
  let feeMap = {};
  fees.forEach(f => feeMap[f.chan] = f);
  fees.forEach(f => {
    let p = prevMap[f.chan];
    if (!p) return logger.log('new channel', f.chan, 'with ', f.name);
    // compare the stats
    let newFee = {};
    if (f.remote.base != p.remote.base) {
      newFee.base = f.remote.base;
      let msg = util.format('channel %s with %s: base changed from %d to %d', f.chan, f.name, p.remote.base, f.remote.base);
      logger.log(msg);
      // format for telegram
      msg = util.format('channel %s with <b>%s</b>: base changed from %d to %d', f.chan, f.name, p.remote.base, f.remote.base);
      sendMessageFormatted(msg);
    }
    if (f.remote.rate != p.remote.rate) {
      newFee.ppm = f.remote.rate;
      let msg = util.format('channel %s with %s: ppm changed from %d to %d', f.chan, f.name, p.remote.rate, f.remote.rate);
      logger.log(msg);
      msg = util.format('channel %s with <b>%s</b>: ppm changed from %d to %d', f.chan, f.name, p.remote.rate, f.remote.rate);
      sendMessageFormatted(msg);
    }
    if (newFee.base || newFee.ppm) {
      // record in the db
      let r = { node:f.id, chan:f.chan };
      if (newFee.base) r.base = newFee.base;
      if (newFee.ppm) r.ppm = newFee.ppm;
      recordFee(r);
      // analyze fees
      if (outboundMap[p.id]) {
        let analysis = analyzeFees(p.name, p.id, p.local, f.remote);
        if (analysis) {
          const action = constants.feeAnalysis.action;
          let status = analysis[0];
          // format telegram msg
          let msg = util.format('channel %s with <b>%s</b>:', p.chan, p.name);
          if (status.action === action.pause) msg += ' rebalancing is paused';
          else msg += ' rebalancing is active';
          if (status.range || status.summary) {
            if (status.range) msg += ', suggested local ppm range: ' + status.range;
            if (status.summary) msg += ', ' + status.summary;
            logger.log(msg);
            sendMessageFormatted(msg);
          } else {
            logger.log(p.name + ': no range or summary found, skipping');
          }
        } else {
          logger.log(p.name + ': analysis not generated, weird, skipping');
        }
      } else {
        logger.log(p.name + ': is not an outbound or balanced peer, skipping');
      }
    }
  })
  // see if any of the channels closed
  prevFees.forEach(f => {
    let p = feeMap[f.chan];
    if (!p) {
      let msg = util.format('channel %s with %s: could not find the fee, the channel was likely closed', f.chan, f.name);
      logger.log(msg);
      msg = util.format('channel %s with <b>%s</b>: could not find the fee, the channel was likely closed', f.chan, f.name);
      sendMessageFormatted(msg);
    }
  })
}

if (global.bot) {
  setInterval(pollMessages, pollInterval * 1000);
}

function pollMessages() {
  try {
    pollMessagesExec();
  } catch(err) {
    logger.log(err.message);
  }
}

// poll messages from the db
function pollMessagesExec() {
  serviceUtils.TelegramBot.recordHeartbeat('poll');
  let list = fetchTelegramMessageSync();
  if (!list || list.length === 0) return;
  logger.log('processing', list.length, 'messages');
  let chatId = getChatId();
  let ids = [];
  try {
    let count = 0;
    list.forEach(m => {
      logger.log('processing message:', stringify(m));
      // make sure that messages are delivered sequentially
      // the best way i found so far to do it is by adding a small
      // interval between each message. just waiting for async promise
      // did not solve the above.
      setTimeout (() => { sendMessageImpl(m.message) }, count++ * 250);
      ids.push(m.id);
    })
  } catch(error) {
    logger.error(error.message);
  } finally {
    deleteTelegramMessages(ids);
  }

  function sendMessageImpl(m) {
    botSend(chatId, m);
  }
}

// The bot's sendMessage returns a promise that rejects on any failed send:
// Telegram unreachable, rate limited, the bot blocked, no chat id yet, bad
// HTML. Left unhandled, that reaches the process-wide handler in api/logger.js,
// which exits, and the service would crash on the next send. Log it instead.
function botSend(chatId, text, opts) {
  if (!chatId || !global.bot) return;
  global.bot.sendMessage(chatId, text, opts)
    .catch(err => logger.error('telegram: could not send message:', err.message));
}

function initBot() {
  logger.log('-----------------------------');
  logger.log('initializing bot');

  if (global.bot) return;
  const token = config.telegramToken;
  if (!token) return logger.error('could not find telegram token. check your config file');

  // initialize
  global.bot = new TelegramBot(token, { polling: true });

  // return help string
  global.bot.onText(/\/help/, (msg, match) => {  
    const chatId = msg.chat.id;
    setChatId(chatId);

    const help = 'Welcome to the Lightning Jet telegram bot!';

    // send back the matched "whatever" to the chat
    botSend(chatId, help);
  })

  global.bot.onText(/\/start/, (msg, match) => {  
    const chatId = msg.chat.id;
    setChatId(chatId);

    // send back the matched "whatever" to the chat
    botSend(chatId, 'started');
  })
}

function setChatId(id) {
  logger.log('setting chat id:', id);
  setPropSync('botChatId', id);
}

function getChatId() {
  return getPropSync('botChatId');
}

function sendMessage(msg) {
  botSend(getChatId(), msg);
}

function sendMessageFormatted(msg) {
  botSend(getChatId(), msg, {parse_mode: 'HTML'});
}
