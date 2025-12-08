apiVersion: apps/v1
kind: Deployment
metadata:
  name: myapp
spec:
  replicas: ${replicas}
  selector:
    matchLabels:
      app: myapp
  template:
    metadata:
      labels:
        app: myapp
    spec:
      containers:
      - name: myapp
        image: myapp:${image_tag}
        ports:
        - containerPort: 8080
