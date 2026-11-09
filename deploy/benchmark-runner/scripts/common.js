// Settings shared by every benchmark. The backend sets these on the Job.
export const url = `${__ENV.TARGET_URL}${__ENV.TARGET_PATH}`;
export const params = { headers: JSON.parse(__ENV.TARGET_HEADERS || '{}') };
export const tags = { capp: __ENV.CAPP_NAME, namespace: __ENV.CAPP_NAMESPACE, cluster: __ENV.CAPP_CLUSTER };
export const isSuccess = (res) => res.status >= 200 && res.status < 400;
