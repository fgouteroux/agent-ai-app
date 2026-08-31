import type { Configuration } from 'webpack';
import { resolve, join, dirname } from 'path';
import { fileURLToPath } from 'url';
import CopyWebpackPlugin from 'copy-webpack-plugin';
import ForkTsCheckerWebpackPlugin from 'fork-ts-checker-webpack-plugin';

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);

const config = (_env: Record<string, string>): Configuration => ({
  context: join(__dirname, 'src'),
  entry: './module.tsx',
  mode: _env.production ? 'production' : 'development',
  // Production builds ship no source map at all -- publishing one exposes
  // the full original source (comments, internal logic included) to
  // anyone who fetches it (security-audit finding M-08). There's no
  // private error-tracking service integrated here to justify
  // hidden-source-map's tradeoff (a map that's generated but not
  // referenced), so plain `false` is both simpler and equally safe.
  devtool: _env.production ? false : 'source-map',
  output: {
    clean: true,
    filename: 'module.js',
    path: resolve(__dirname, 'dist'),
    publicPath: '',
    libraryTarget: 'amd',
    uniqueName: 'shortbobcat2735-agentai-app',
  },
  externals: [
    'lodash',
    'react',
    'react-dom',
    'react/jsx-runtime',
    'react/jsx-dev-runtime',
    '@grafana/data',
    '@grafana/runtime',
    '@grafana/ui',
  ],
  resolve: {
    extensions: ['.ts', '.tsx', '.js', '.jsx'],
  },
  module: {
    rules: [
      {
        test: /\.tsx?$/,
        use: {
          loader: 'swc-loader',
        },
        exclude: /node_modules/,
      },
      {
        test: /\.css$/,
        use: ['style-loader', 'css-loader'],
      },
    ],
  },
  performance: {
    hints: false,
  },
  plugins: [
    new CopyWebpackPlugin({
      patterns: [
        { from: 'plugin.json', to: '.' },
        { from: 'img/', to: 'img/' },
        { from: '../README.md', to: '.', noErrorOnMissing: true },
        { from: '../LICENSE', to: '.', noErrorOnMissing: true },
        { from: '../CHANGELOG.md', to: '.', noErrorOnMissing: true },
      ],
    }),
    new ForkTsCheckerWebpackPlugin({
      async: Boolean(_env.development),
      typescript: { configFile: resolve(__dirname, 'tsconfig.json') },
    }),
  ],
});

export default config;
