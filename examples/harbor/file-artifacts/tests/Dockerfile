FROM python:3.12-slim
WORKDIR /app
COPY . /tests/
RUN chmod +x /tests/test.sh
CMD ["/tests/test.sh"]
