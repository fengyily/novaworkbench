// zh-CN — the canonical resource tree. Every other locale must mirror this
// key-for-key (assertSameKeys at init in dev) and CustomTypeOptions derives
// `t()`'s key union from THIS file.
import common from './modules/common';
import nav from './modules/nav';
import login from './modules/login';
import status from './modules/status';
import errors from './modules/errors';
import time from './modules/time';
import dashboard from './modules/dashboard';
import projects from './modules/projects';
import requirements from './modules/requirements';
import wizard from './modules/wizard';
import knowledge from './modules/knowledge';
import chat from './modules/chat';
import reports from './modules/reports';
import schedules from './modules/schedules';
import settings from './modules/settings';
import components from './modules/components';


const zhCN = {
  common,
  nav,
  login,
  status,
  errors,
  time,
  dashboard,
  projects,
  requirements,
  wizard,
  knowledge,
  chat,
  reports,
  schedules,
  settings,

  components,
};

export default zhCN;
