const config = require('../api/config');
const logger = require('../api/logger');
const findProc = require('find-process');
const {spawnDetached} = require('../api/utils');
const {sendMessage} = require('../api/telegram');
const {setPropSync} = require('../db/utils');
const {getPropAndDateSync} = require('../db/utils');

const testModeOn = global.testModeOn;
const stringify = obj => JSON.stringify(obj, null, 2);

class Service {
  stop() {
    if (!this.isRunning()) return console.log('already stopped');
    stopServiceExec(this.proc);
    sendMessage('stopped ' + this.name);
  }
  restart() {
    if (this.isRunning()) this.stop();
    return this.start();
  }
  isRunning() {
    return isServiceRunning(this.proc);
  }
  status() {
    if (this.isRunning()) return 'running';
    else return 'stopped';
  }
  exists() {
    return require('fs').existsSync(this.path());
  }
  start() {
    if (this.isRunning()) return console.log('already running');
    if (!this.exists()) return 'service does not exist';
    if (!this.isConfigured()) {
      if (this.help) return 'service is not configured, can\'t start it. ' + this.help;
      else return 'service is not configured';
    }
    if (this.isDisabled()) return console.log('service is disabled');

    // spawn the process with the binary this one runs on; a bare `node` searches
    // PATH, which outside a login shell can find an older system node that the
    // native modules weren't built for
    const p = {
      cmd: process.execPath,
      arg: [ this.path() ],
      log: this.log
    }
    logger.debug('spawning process', stringify(p));
    if (testModeOn) logger.debug('test mode on, skip spawning');
    else spawnDetached(p);

    sendMessage('started ' + this.name);
    return 'started';
  }
  path() {
    return __dirname + '/' + this.proc;
  }
  isConfigured() {
    return true;  // services can overrides this if configuration is required
  }
  isDisabled() {
    return false;
  }
  recordHeartbeat(sub) {
    let prop = 'service.' + this.name + '.heartbeat';
    if (sub) prop += '.' + sub;
    setPropSync(prop, Date.now());
  }
  lastHeartbeat(sub) {
    let prop = 'service.' + this.name + '.heartbeat';
    if (sub) prop += '.' + sub;
    let val = getPropAndDateSync(prop);
    return val && val.date;
  }
}

class Rebalancer extends Service {
  static name = 'rebalancer';
  constructor() {
    super();
    this.name = Rebalancer.name;
    this.proc = 'rebalancer.js';
    this.log = '/tmp/jet-rebalancer.log';
  }
  isDisabled() {
    return config.rebalancer.disabled;
  }
}

class HtlcLogger extends Service {
  static name = 'logger';
  constructor() {
    super();
    this.name = HtlcLogger.name;
    this.proc = 'htlc-logger.js';
    this.log = '/tmp/jet-logger.log';
  }
}

// watchdog daemon; responsible for keeping all other services alive.
class Launcher extends Service {
  static name = 'daddy';
  constructor() {
    super();
    this.name = Launcher.name;
    this.proc = 'launcher.js';
    this.log = '/tmp/jet-daddy.log';
  }
}

// worker daemon; populates db tables, periodic bos reconnect, etc.
class Worker extends Service {
  static name = 'worker';
  constructor() {
    super();
    this.name = Worker.name;
    this.proc = 'worker.js';
    this.log = '/tmp/jet-worker.log';
  }
}

// telegram bot
class TelegramBot extends Service {
  static name = 'telegram';
  constructor() {
    super();
    this.name = TelegramBot.name;
    this.proc = 'telegram.js';
    this.log = '/tmp/jet-telegram.log';
    this.help = 'https://github.com/itsneski/lightning-jet#telegram-bot';
  }

  isConfigured() {
    return config.telegramToken;
  }
}

const serviceNames = [
  Rebalancer.name,
  HtlcLogger.name,
  Launcher.name,
  TelegramBot.name,
  Worker.name
]

var services = {};
services[Rebalancer.name] = new Rebalancer();
services[HtlcLogger.name] = new HtlcLogger();
services[Launcher.name] = new Launcher();
services[TelegramBot.name] = new TelegramBot();
services[Worker.name] = new Worker();

module.exports = {
  Rebalancer: services[Rebalancer.name],
  HtlcLogger: services[HtlcLogger.name],
  Launcher: services[Launcher.name],
  TelegramBot: services[TelegramBot.name],
  Worker: services[Worker.name],
  getServiceNames: function() { 
    return serviceNames;
  },
  isRunning: function(name) {
    if (!name) return console.error('missing service');
    if (!services[name]) return console.error('unknown service:', name);
    return services[name].isRunning();
  },
  isDisabled: function(name) {
    if (!name) return console.error('missing service');
    if (!services[name]) return console.error('unknown service:', name);
    return services[name].isDisabled();
  },
  stopService: function(name) {
    if (!name) return console.error('missing service');
    if (name === 'all') {
      const daddy = module.exports.Launcher;
      daddy.stop();
      Object.values(services).forEach(s => {
        if (s.name !== daddy.name) s.stop();
      })
    } else {
      if (!services[name]) return console.error('unknown service:', name);
      return services[name].stop();
    }
  },
  startService: function(name) {
    if (!name) return console.error('missing service');
    if (name === 'all') {
      const daddy = module.exports.Launcher;
      daddy.start();  // will start all other services
    } else {
      if (!services[name]) return console.error('unknown service:', name);
      return services[name].start();
    }
  },
  restartService: function(name) {
    if (!name) return 'missing service';
    if (name === 'all') {
      module.exports.stopService('all');
      module.exports.startService('all');
    } else {
      if (!services[name]) return 'unknown service: ' + name;
      return services[name].restart();
    }
  },
  printStatus: function() {
    let status = [];
    Object.values(services).forEach(s => {
      status.push({service: s.name, status: s.status(), log: s.log});
    })
    console.table(status);
  },
  isConfigured: function(name) {
    if (!name) return 'missing service';
    if (!services[name]) return 'unknown service: ' + name;
    return services[name].isConfigured();
  }
}

function stopServiceExec(service) {
  let res;
  findProc('name', service).then(function (list) {
    res = list;
  })
  while(res === undefined) {
    require('deasync').runLoopOnce();
  }
  let pid = res[0] && res[0].pid;
  if (!pid) return 'already stopped';
  process.kill(pid);
  return 'stopped';
}

function isServiceRunning(service) {
  let res;
  findProc('name', service).then(list => res = list);
  while(res === undefined) {
    require('deasync').runLoopOnce();
  }
  return res.length > 0;
}
