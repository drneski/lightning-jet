const importLazy = require('import-lazy')(require);

const pay = importLazy('../../api/pay');

const {
  requireLndAlive,
} = require('../utils/lnd');

function registerPayCommand(program) {
  program
    .command('pay')
    .summary('Pay an invoice')
    .description('Pay a Lightning invoice.')
    .argument('<request>', 'Payment Request')
    // one value per flag, repeatable, as in 1.6: a variadic option would also
    // swallow the payment request that follows it
    .option('--avoid <avoid>', 'Avoid forwarding via node/chan/tag (repeat for more)', collect)
    .option('--in <peer>', 'Route through specific peer of destination')
    .option('--max-fee <sats>', 'Maximum fee to pay', parsePositiveInt, 1337)
    .option('--max-paths <paths>', 'Maximum paths to use', parsePositiveInt, 1)
    .option('--message <message>', 'Attach text message to payment')
    .option('--no-color', 'Mute all colors')
    .option('--node <node>', 'Node to use to pay payment request')
    .option('--out <peer>', 'Make first hop through peer (repeat for more)', collect)
    .action(async (request, options) => {
      requireLndAlive();

      const ret = pay({
        request,
        avoid: options.avoid || [],
        in: options.in,
        maxFee: options.maxFee,
        maxPaths: options.maxPaths,
        message: options.message,
        node: options.node,
        out: options.out || [],
        color: options.color,
      });

      console.log(ret);
    });
}

function collect(value, previous) {
  return (previous || []).concat([value]);
}

function parsePositiveInt(value) {
  const parsed = Number(value);

  if (!Number.isInteger(parsed) || parsed <= 0) {
    throw new Error('Expected a positive integer');
  }

  return parsed;
}

module.exports = {
  registerPayCommand,
};
