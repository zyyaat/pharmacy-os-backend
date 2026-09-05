/** @type {import('next').NextConfig} */
const nextConfig = {
  allowedDevOrigins: ['127.0.0.1', 'localhost', process.env.REPLIT_DEV_DOMAIN].filter(Boolean),
  async rewrites() {
    return [
      {
        source: '/api/v1/:path*',
        destination: 'http://127.0.0.1:8080/api/v1/:path*',
      },
      {
        source: '/health',
        destination: 'http://127.0.0.1:8080/health',
      },
    ]
  },
}

module.exports = nextConfig
