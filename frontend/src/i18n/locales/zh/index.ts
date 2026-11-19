import requestSpeed from './requestSpeed'
import deepseekTools from './deepseekTools'
import upstreamRejection from './upstreamRejection'
import pendingCharges from './pendingCharges'
import adminAccessKeys from './adminAccessKeys'
import mcp from './mcp'
import landing from './landing'
import common from './common'
import dashboard from './dashboard'
import channelMonitorV2 from './channelMonitorV2'
import batchImage from './batchImage'
import admin from './admin'
import misc from './misc'
import sourcesBalance from './sourcesBalance'

export default {
  requestSpeed,
  deepseekTools,
  ...upstreamRejection,
  ...pendingCharges,
  adminAccessKeys,
  mcp,
  ...landing,
  ...common,
  ...dashboard,
  ...channelMonitorV2,
  ...batchImage,
  admin,
  ...misc,
  ...sourcesBalance,
}
